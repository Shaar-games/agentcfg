// Package updater downloads a verified GitHub Release and replaces the
// currently running AgentMux executable. It never reads or edits MCP settings.
package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	latestAPIURL    = "https://api.github.com/repos/Shaar-games/agentmux/releases/latest"
	releaseBaseURL  = "https://github.com/Shaar-games/agentmux/releases/download"
	maxArchiveBytes = 256 << 20
	maxBinaryBytes  = 128 << 20
	maxManifestSize = 64 << 10
)

var releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// Options allows self-update to be tested against a private HTTP fixture.
// An empty executable path always means the currently running binary.
type Options struct {
	Executable     string
	CurrentVersion string
	LatestAPI      string
	DownloadBase   string
}

type Result struct {
	Version        string
	Path           string
	AlreadyCurrent bool
	// On Windows, a renamed executable may remain mapped by the current
	// process. In that case the previous binary cannot be deleted until exit.
	BackupPath string
}

type latestRelease struct {
	TagName         string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish"`
	Draft           bool   `json:"draft"`
	Prerelease      bool   `json:"prerelease"`
}

// Update installs the latest published stable release. GitHub's release
// workflow builds these tags from main. Only the exact expected archive and
// checksum manifest are downloaded; the new binary must report that tag.
func Update(ctx context.Context, options Options) (Result, error) {
	executable := options.Executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return Result{}, err
		}
	}
	executable, err := filepath.Abs(executable)
	if err != nil {
		return Result{}, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return Result{}, fmt.Errorf("locate installed executable: %w", err)
	}
	info, err := os.Lstat(executable)
	if err != nil || !info.Mode().IsRegular() {
		return Result{}, fmt.Errorf("installed executable is not a regular file: %s: %v", executable, err)
	}
	originalDigest, err := executableHash(executable)
	if err != nil {
		return Result{}, fmt.Errorf("hash installed executable: %w", err)
	}
	apiURL := options.LatestAPI
	if apiURL == "" {
		apiURL = latestAPIURL
	}
	base := options.DownloadBase
	if base == "" {
		base = releaseBaseURL
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	version, err := findLatest(ctx, client, apiURL)
	if err != nil {
		return Result{}, err
	}
	result := Result{Version: version, Path: executable}
	if options.CurrentVersion == version || stableVersionNotOlder(options.CurrentVersion, version) {
		result.AlreadyCurrent = true
		return result, nil
	}
	asset, filename, err := platformAsset()
	if err != nil {
		return Result{}, err
	}
	downloadRoot, err := os.MkdirTemp("", "agentmux-update-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(downloadRoot)
	downloadURL := strings.TrimRight(base, "/") + "/" + version + "/"
	manifest, err := getSmall(ctx, client, downloadURL+"SHA256SUMS", maxManifestSize)
	if err != nil {
		return Result{}, fmt.Errorf("download SHA256SUMS: %w", err)
	}
	expected, err := checksumFor(manifest, asset)
	if err != nil {
		return Result{}, err
	}
	archivePath := filepath.Join(downloadRoot, asset)
	if err := downloadArchive(ctx, client, downloadURL+asset, archivePath, expected); err != nil {
		return Result{}, err
	}
	stage, err := os.CreateTemp(filepath.Dir(executable), ".agentmux-update-*"+filepath.Ext(executable))
	if err != nil {
		return Result{}, fmt.Errorf("stage executable beside installation: %w", err)
	}
	stagePath := stage.Name()
	defer os.Remove(stagePath)
	if err := unpackVerified(archivePath, filename, stage); err != nil {
		_ = stage.Close()
		return Result{}, err
	}
	if err := stage.Sync(); err != nil {
		_ = stage.Close()
		return Result{}, err
	}
	if err := stage.Close(); err != nil {
		return Result{}, err
	}
	if err := os.Chmod(stagePath, 0o755); err != nil {
		return Result{}, err
	}
	if err := verifyVersion(ctx, stagePath, version); err != nil {
		return Result{}, fmt.Errorf("staged binary failed validation (current executable unchanged): %w", err)
	}
	backup, err := replaceExecutable(ctx, executable, stagePath, version, originalDigest)
	if err != nil {
		return Result{}, err
	}
	result.BackupPath = backup
	return result, nil
}

// Avoid accidentally downgrading when GitHub's latest-release endpoint is
// stale or the local installation already has a newer stable release.
func stableVersionNotOlder(installed, latest string) bool {
	if !releaseTag.MatchString(installed) || !releaseTag.MatchString(latest) {
		return false
	}
	left := strings.Split(strings.TrimPrefix(installed, "v"), ".")
	right := strings.Split(strings.TrimPrefix(latest, "v"), ".")
	for i := 0; i < 3; i++ {
		a, errA := strconv.ParseUint(left[i], 10, 64)
		b, errB := strconv.ParseUint(right[i], 10, 64)
		if errA != nil || errB != nil {
			return false
		}
		if a > b {
			return true
		}
		if a < b {
			return false
		}
	}
	return true
}

func findLatest(ctx context.Context, client *http.Client, apiURL string) (string, error) {
	data, err := getSmall(ctx, client, apiURL, 1<<20)
	if err != nil {
		return "", fmt.Errorf("get latest AgentMux release: %w", err)
	}
	var release latestRelease
	if err := json.Unmarshal(data, &release); err != nil {
		return "", fmt.Errorf("invalid GitHub release response: %w", err)
	}
	if !releaseTag.MatchString(release.TagName) || release.Draft || release.Prerelease {
		return "", errors.New("latest release is missing a valid stable version tag")
	}
	if release.TargetCommitish != "main" {
		return "", fmt.Errorf("latest release %s does not target main", release.TagName)
	}
	return release.TagName, nil
}

func platformAsset() (archive, binary string, err error) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return "", "", fmt.Errorf("unsupported CPU architecture: %s", runtime.GOARCH)
	}
	binary = "agentmux"
	switch runtime.GOOS {
	case "windows":
		binary = "agentmux.exe"
		archive = "agentmux_windows_" + runtime.GOARCH + ".zip"
	case "darwin", "linux":
		archive = "agentmux_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	default:
		return "", "", fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
	return archive, binary, nil
}

func getSmall(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "agentmux-updater")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d", url, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download exceeds %d bytes", limit)
	}
	return data, nil
}

func checksumFor(manifest []byte, asset string) ([32]byte, error) {
	var expected [32]byte
	found := false
	for _, line := range strings.Split(string(manifest), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		if found {
			return expected, fmt.Errorf("duplicate checksum for %s", asset)
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != len(expected) {
			return expected, fmt.Errorf("invalid SHA-256 for %s", asset)
		}
		copy(expected[:], decoded)
		found = true
	}
	if !found {
		return expected, fmt.Errorf("missing checksum for %s", asset)
	}
	return expected, nil
}

func downloadArchive(ctx context.Context, client *http.Client, url, path string, expected [32]byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "agentmux-updater")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download release asset: HTTP %d", response.StatusCode)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(response.Body, maxArchiveBytes+1))
	if err != nil {
		return err
	}
	if size > maxArchiveBytes {
		return fmt.Errorf("release archive exceeds %d bytes", maxArchiveBytes)
	}
	if got := hasher.Sum(nil); !equalDigest(got, expected[:]) {
		return errors.New("release archive SHA-256 mismatch; executable unchanged")
	}
	return file.Sync()
}

func equalDigest(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func unpackVerified(archivePath, filename string, output io.Writer) error {
	if strings.HasSuffix(archivePath, ".zip") {
		z, err := zip.OpenReader(archivePath)
		if err != nil {
			return err
		}
		defer z.Close()
		if len(z.File) != 1 || z.File[0].Name != filename || !z.File[0].Mode().IsRegular() || z.File[0].UncompressedSize64 > maxBinaryBytes {
			return errors.New("release ZIP must contain only a regular agentmux executable")
		}
		reader, err := z.File[0].Open()
		if err != nil {
			return err
		}
		defer reader.Close()
		return copyBounded(output, reader)
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	header, err := tarReader.Next()
	if err != nil {
		return err
	}
	if header.Name != filename || header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || header.Size <= 0 || header.Size > maxBinaryBytes {
		return errors.New("release tarball must start with a regular agentmux executable")
	}
	if err := copyBounded(output, tarReader); err != nil {
		return err
	}
	if _, err := tarReader.Next(); err != io.EOF {
		return errors.New("release tarball contains unexpected extra entries or is invalid")
	}
	return nil
}

func copyBounded(writer io.Writer, reader io.Reader) error {
	size, err := io.Copy(writer, io.LimitReader(reader, maxBinaryBytes+1))
	if err != nil {
		return err
	}
	if size <= 0 || size > maxBinaryBytes {
		return fmt.Errorf("extracted executable must be between 1 and %d bytes", maxBinaryBytes)
	}
	return nil
}

func verifyVersion(ctx context.Context, path, version string) error {
	timeout, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(timeout, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("execute version smoke test: %w", err)
	}
	if got := strings.TrimSpace(string(output)); got != version {
		return fmt.Errorf("version reported %q, expected %q", got, version)
	}
	return nil
}
