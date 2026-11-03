package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

const oldVersion = "v0.1.0"
const newVersion = "v9.9.9"

func buildFixtureBinary(t *testing.T, version string) []byte {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	filename := "agentmux"
	if runtime.GOOS == "windows" {
		filename += ".exe"
	}
	path := filepath.Join(t.TempDir(), filename)
	command := exec.Command("go", "build", "-trimpath", "-ldflags", "-X main.version="+version, "-o", path, "./cmd/agentmux")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s fixture: %v\n%s", version, err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func archiveFixture(t *testing.T, filename string, binary []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if runtime.GOOS == "windows" {
		writer := zip.NewWriter(&buffer)
		header := &zip.FileHeader{Name: filename, Method: zip.Deflate}
		header.SetMode(0o755)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(binary); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		gz := gzip.NewWriter(&buffer)
		writer := tar.NewWriter(gz)
		if err := writer.WriteHeader(&tar.Header{Name: filename, Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(binary); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buffer.Bytes()
}

type releaseFixture struct {
	tag     string
	branch  string
	archive []byte
	badSHA  bool
	hits    atomic.Int32
}

func serveRelease(t *testing.T, asset string, release *releaseFixture) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"tag_name":%q,"target_commitish":%q,"draft":false,"prerelease":false}`, release.tag, release.branch)
		case "/downloads/" + release.tag + "/SHA256SUMS":
			release.hits.Add(1)
			sum := sha256.Sum256(release.archive)
			if release.badSHA {
				sum[0] ^= 0xff
			}
			_, _ = fmt.Fprintf(w, "%x  %s\n", sum, asset)
		case "/downloads/" + release.tag + "/" + asset:
			release.hits.Add(1)
			_, _ = w.Write(release.archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func installFixture(t *testing.T, binary []byte) string {
	t.Helper()
	name := "agentmux"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSelfUpdateVerifiedReplacementAndFailurePreservation(t *testing.T) {
	oldBinary := buildFixtureBinary(t, oldVersion)
	newBinary := buildFixtureBinary(t, newVersion)
	asset, filename, err := platformAsset()
	if err != nil {
		t.Fatal(err)
	}
	archive := archiveFixture(t, filename, newBinary)
	for _, tc := range []struct {
		name          string
		archiveBinary []byte
		invalidHash   bool
		branch        string
		current       string
		wantError     bool
		noDownload    bool
	}{
		{name: "update", branch: "main", current: oldVersion},
		{name: "already-current", branch: "main", current: newVersion, noDownload: true},
		{name: "installed-newer-than-github-latest", branch: "main", current: "v9.10.0", noDownload: true},
		{name: "bad-sha", branch: "main", current: oldVersion, invalidHash: true, wantError: true},
		{name: "wrong-branch", branch: "not-main", current: oldVersion, wantError: true, noDownload: true},
		{name: "wrong-executable-version", branch: "main", current: oldVersion, archiveBinary: oldBinary, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installed := installFixture(t, oldBinary)
			releaseArchive := archive
			if tc.archiveBinary != nil {
				releaseArchive = archiveFixture(t, filename, tc.archiveBinary)
			}
			fixture := &releaseFixture{tag: newVersion, branch: tc.branch, archive: releaseArchive, badSHA: tc.invalidHash}
			server := serveRelease(t, asset, fixture)
			result, err := Update(context.Background(), Options{
				Executable: installed, CurrentVersion: tc.current,
				LatestAPI: server.URL + "/latest", DownloadBase: server.URL + "/downloads",
			})
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v; expected failure %v", err, tc.wantError)
			}
			if tc.noDownload && fixture.hits.Load() != 0 {
				t.Fatal("release assets were downloaded unexpectedly")
			}
			if tc.name == "update" {
				actualInfo, actualErr := os.Stat(result.Path)
				installedInfo, installedErr := os.Stat(installed)
				if actualErr != nil || installedErr != nil {
					t.Fatalf("updated file missing: %v, %v", actualErr, installedErr)
				}
				if result.Version != newVersion || result.AlreadyCurrent || !os.SameFile(actualInfo, installedInfo) {
					t.Fatalf("update result = %+v", result)
				}
				if err := verifyVersion(context.Background(), installed, newVersion); err != nil {
					t.Fatal(err)
				}
			} else {
				if !bytes.Equal(readFixture(t, installed), oldBinary) {
					t.Fatalf("failed or skipped update modified existing executable")
				}
				if tc.noDownload && !result.AlreadyCurrent && tc.branch == "main" {
					t.Fatal("expected already-current result")
				}
			}
			if result.BackupPath != "" {
				// The Go test process does not have this old fixture mapped, so
				// Windows backup cleanup should succeed.
				t.Fatalf("unexpected retained backup: %s", result.BackupPath)
			}
		})
	}
}

func TestStableVersionOrdering(t *testing.T) {
	for _, tc := range []struct {
		local, latest string
		shouldSkip    bool
	}{
		{"v1.2.10", "v1.2.9", true},
		{"v2.0.0", "v1.99.99", true},
		{"v1.2.3", "v1.2.3", true},
		{"v1.2.3", "v1.2.4", false},
		{"dev", "v1.0.0", false},
		{"v1.2.3-rc1", "v1.2.3", false},
	} {
		if got := stableVersionNotOlder(tc.local, tc.latest); got != tc.shouldSkip {
			t.Fatalf("installed %s, latest %s: got %v, want %v", tc.local, tc.latest, got, tc.shouldSkip)
		}
	}
}

func TestSelfUpdateWhileExecutableIsRunning(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	asset, filename, err := platformAsset()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	binary := filepath.Join(directory, filename)
	helper := exec.Command("go", "build", "-trimpath", "-o", binary, "./internal/updater/testdata/selfupdate")
	helper.Dir = root
	if output, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("build self-update fixture: %v\n%s", err, output)
	}
	newBinary := buildFixtureBinary(t, newVersion)
	fixture := &releaseFixture{tag: newVersion, branch: "main", archive: archiveFixture(t, filename, newBinary)}
	server := serveRelease(t, asset, fixture)
	command := exec.Command(binary)
	command.Env = append(os.Environ(),
		"AGENTMUX_TEST_LATEST_API="+server.URL+"/latest",
		"AGENTMUX_TEST_DOWNLOAD_BASE="+server.URL+"/downloads",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("live self-update failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "updated "+newVersion) {
		t.Fatalf("unexpected updater output: %s", output)
	}
	if err := verifyVersion(context.Background(), binary, newVersion); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseValidationAndArchiveConstraints(t *testing.T) {
	asset, filename, err := platformAsset()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"", "junk", "v1.2.3/../evil", "v1.2.3-pre"} {
		if raw != "" && releaseTag.MatchString(raw) && !strings.Contains(raw, "/") {
			continue
		}
		release := &releaseFixture{tag: raw, branch: "main", archive: []byte("fake")}
		server := serveRelease(t, asset, release)
		_, err := findLatest(context.Background(), http.DefaultClient, server.URL+"/latest")
		if err == nil {
			t.Fatalf("release tag %q accepted", raw)
		}
	}
	for _, manifest := range []string{
		"missing\n",
		fmt.Sprintf("%064x  %s\n%064x  %s\n", 1, asset, 2, asset),
		fmt.Sprintf("deadbeef  %s\n", asset),
	} {
		if _, err := checksumFor([]byte(manifest), asset); err == nil {
			t.Fatalf("bad checksum manifest accepted: %q", manifest)
		}
	}
	archive := archiveFixture(t, filename, []byte("not an executable"))
	name := filepath.Join(t.TempDir(), asset)
	if err := os.WriteFile(name, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := unpackVerified(name, filename, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, &output); err != nil {
		t.Fatal(err)
	}
}
