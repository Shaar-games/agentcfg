package installtest

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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
	"testing"
)

func root(t *testing.T) string {
	t.Helper()
	result, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func fixtureServer(t *testing.T, asset string, data []byte, badHash bool) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(data)
	if badHash {
		sum[0] ^= 255
	}
	manifest := fmt.Sprintf("%x  %s\n", sum, asset)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case asset:
			_, _ = w.Write(data)
		case "SHA256SUMS":
			_, _ = io.WriteString(w, manifest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func windowsArchive(t *testing.T) []byte {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "agentmux.exe")
	command := exec.Command("go", "build", "-o", binary, "./cmd/agentmux")
	command.Dir = root(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build release fixture: %v\n%s", err, output)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	entry, err := writer.Create("agentmux.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = entry.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func bashArchive(t *testing.T) []byte {
	t.Helper()
	var data []byte
	if runtime.GOOS == "windows" {
		data = []byte("#!/usr/bin/env bash\nif [[ \"$1\" == version ]]; then echo fixture; else exit 1; fi\n")
	} else {
		binary := filepath.Join(t.TempDir(), "agentmux")
		command := exec.Command("go", "build", "-o", binary, "./cmd/agentmux")
		command.Dir = root(t)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build release fixture: %v\n%s", err, output)
		}
		var err error
		data, err = os.ReadFile(binary)
		if err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	zipped := gzip.NewWriter(&buf)
	writer := tar.NewWriter(zipped)
	if err := writer.WriteHeader(&tar.Header{Name: "agentmux", Mode: 0755, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPowerShellDownloadsAndVerifiesRelease(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell Windows fixture only")
	}
	shell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe is unavailable")
	}
	data := windowsArchive(t)
	for _, bad := range []bool{false, true} {
		name := "valid"
		if bad {
			name = "invalid-sha256"
		}
		t.Run(name, func(t *testing.T) {
			server := fixtureServer(t, "agentmux_windows_"+runtime.GOARCH+".zip", data, bad)
			dir := filepath.Join(t.TempDir(), "bin")
			args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(root(t), "install.ps1")}
			environment := append(os.Environ(), "AGENTMUX_VERSION=latest", "AGENTMUX_RELEASE_BASE_URL="+server.URL, "AGENTMUX_BIN_DIR="+dir)
			command := exec.Command(shell, args...)
			command.Env = environment
			output, err := command.CombinedOutput()
			binary := filepath.Join(dir, "agentmux.exe")
			if bad {
				if err == nil || !strings.Contains(string(output), "SHA-256 mismatch") {
					t.Fatalf("bad SHA-256 not rejected: %v\n%s", err, output)
				}
				if _, err = os.Stat(binary); !os.IsNotExist(err) {
					t.Fatal("corrupt binary was installed")
				}
				return
			}
			if err != nil {
				t.Fatalf("PowerShell installation failed: %v\n%s", err, output)
			}
			if _, err = exec.Command(binary, "version").CombinedOutput(); err != nil {
				t.Fatalf("installed executable cannot run: %v", err)
			}
			again := exec.Command(shell, args...)
			again.Env = environment
			if output, err = again.CombinedOutput(); err == nil || !strings.Contains(string(output), "Refusing to overwrite") {
				t.Fatalf("existing binary overwritten: %v\n%s", err, output)
			}
		})
	}
}

func TestBashDownloadsAndVerifiesRelease(t *testing.T) {
	bash := "bash"
	if runtime.GOOS == "windows" {
		bash = filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe")
	}
	if _, err := os.Stat(bash); runtime.GOOS == "windows" && err != nil {
		t.Skip("Git Bash not available")
	}
	if runtime.GOOS != "windows" {
		if _, err := exec.LookPath(bash); err != nil {
			t.Skip("Bash not available")
		}
	}
	osName, arch := runtime.GOOS, runtime.GOARCH
	if runtime.GOOS == "windows" {
		osName, arch = "linux", "amd64"
	}
	asset := "agentmux_" + osName + "_" + arch + ".tar.gz"
	data := bashArchive(t)
	for _, bad := range []bool{false, true} {
		name := "valid"
		if bad {
			name = "invalid-sha256"
		}
		t.Run(name, func(t *testing.T) {
			server := fixtureServer(t, asset, data, bad)
			dir := filepath.Join(t.TempDir(), "bin")
			args := []string{filepath.Join(root(t), "install.sh")}
			environment := append(os.Environ(), "AGENTMUX_VERSION=latest", "AGENTMUX_RELEASE_BASE_URL="+server.URL, "AGENTMUX_BIN_DIR="+dir)
			newCommand := func() *exec.Cmd { return exec.Command(bash, args...) }
			if runtime.GOOS == "windows" {
				fake := t.TempDir()
				command := filepath.Join(fake, "uname")
				script := "#!/usr/bin/env bash\nif [[ \"$1\" == '-s' ]]; then echo Linux; else echo x86_64; fi\n"
				if err := os.WriteFile(command, []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
				// Git Bash prepends its system directories to PATH on startup;
				// put the test-only uname first from inside Bash itself.
				newCommand = func() *exec.Cmd {
					return exec.Command(bash, "-c", "PATH=\"$(cygpath -u \"$1\"):$PATH\"; shift; exec bash \"$@\"", "--", fake, args[0])
				}
			}
			command := newCommand()
			command.Env = environment
			output, err := command.CombinedOutput()
			installed := filepath.Join(dir, "agentmux")
			if bad {
				if err == nil || !strings.Contains(string(output), "SHA-256 mismatch") {
					t.Fatalf("bad SHA-256 not rejected: %v\n%s", err, output)
				}
				if _, err = os.Stat(installed); !os.IsNotExist(err) {
					t.Fatal("corrupt binary was installed")
				}
				return
			}
			if err != nil {
				t.Fatalf("Bash installation failed: %v\n%s", err, output)
			}
			if _, err = os.Stat(installed); err != nil {
				t.Fatal(err)
			}
			again := newCommand()
			again.Env = environment
			if output, err = again.CombinedOutput(); err == nil || !strings.Contains(string(output), "Refusing to overwrite") {
				t.Fatalf("existing binary overwritten: %v\n%s", err, output)
			}
		})
	}
}
