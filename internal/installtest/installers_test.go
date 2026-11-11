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

func powerShellFixtureCommand(t *testing.T, shell string, args, environment []string) *exec.Cmd {
	t.Helper()
	fixture := t.TempDir()
	home := filepath.Join(fixture, "home")
	local := filepath.Join(home, "AppData", "Local")
	roaming := filepath.Join(home, "AppData", "Roaming")
	temp := filepath.Join(fixture, "tmp")
	modules := filepath.Join(fixture, "modules")
	cache := filepath.Join(local, "Microsoft", "Windows", "PowerShell", "ModuleAnalysisCache")
	xdgConfig, xdgCache := filepath.Join(fixture, "config"), filepath.Join(fixture, "cache")
	xdgData, xdgState := filepath.Join(fixture, "data"), filepath.Join(fixture, "state")
	xdgRuntime := filepath.Join(fixture, "runtime")
	for _, directory := range []string{home, local, roaming, temp, modules, filepath.Dir(cache), xdgConfig, xdgCache, xdgData, xdgState, xdgRuntime} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command(shell, args...)
	command.Dir = fixture
	volume := filepath.VolumeName(home)
	command.Env = append(append([]string(nil), environment...),
		"HOME="+home, "USERPROFILE="+home, "HOMEDRIVE="+volume, "HOMEPATH="+strings.TrimPrefix(home, volume),
		"APPDATA="+roaming, "LOCALAPPDATA="+local,
		"TMP="+temp, "TEMP="+temp, "TMPDIR="+temp,
		"XDG_CONFIG_HOME="+xdgConfig, "XDG_CACHE_HOME="+xdgCache,
		"XDG_DATA_HOME="+xdgData, "XDG_STATE_HOME="+xdgState, "XDG_RUNTIME_DIR="+xdgRuntime,
		// Windows PowerShell can get an empty LocalApplicationData known folder
		// for a temporary profile and otherwise write its cache relative to cwd.
		"PSModuleAnalysisCachePath="+cache,
		"PSModulePath="+modules+string(os.PathListSeparator)+filepath.Join(filepath.Dir(shell), "Modules"),
	)
	return command
}

func windowsArchive(t *testing.T) []byte {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "agentcfg.exe")
	command := exec.Command("go", "build", "-o", binary, "./cmd/agentcfg")
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
	entry, err := writer.Create("agentcfg.exe")
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
		binary := filepath.Join(t.TempDir(), "agentcfg")
		command := exec.Command("go", "build", "-o", binary, "./cmd/agentcfg")
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
	if err := writer.WriteHeader(&tar.Header{Name: "agentcfg", Mode: 0755, Size: int64(len(data))}); err != nil {
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
			server := fixtureServer(t, "agentcfg_windows_"+runtime.GOARCH+".zip", data, bad)
			dir := filepath.Join(t.TempDir(), "bin")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "agentcfg.exe")
			if bad {
				if err := os.WriteFile(binary, []byte("previous executable"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(root(t), "install.ps1")}
			environment := append(os.Environ(), "AGENTCFG_VERSION=latest", "AGENTCFG_RELEASE_BASE_URL="+server.URL, "AGENTCFG_BIN_DIR="+dir)
			command := powerShellFixtureCommand(t, shell, args, environment)
			output, err := command.CombinedOutput()
			if bad {
				if err == nil || !strings.Contains(string(output), "SHA-256 mismatch") {
					t.Fatalf("bad SHA-256 not rejected: %v\n%s", err, output)
				}
				if existing, readErr := os.ReadFile(binary); readErr != nil || string(existing) != "previous executable" {
					t.Fatalf("bad SHA-256 corrupted existing executable: %q (%v)", existing, readErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PowerShell installation failed: %v\n%s", err, output)
			}
			if _, err = exec.Command(binary, "version").CombinedOutput(); err != nil {
				t.Fatalf("installed executable cannot run: %v", err)
			}
			if help, err := exec.Command(binary).CombinedOutput(); err != nil || !bytes.Contains(help, []byte("Usage: agentcfg")) {
				t.Fatalf("no-argument help failed: %v\n%s", err, help)
			}
			again := powerShellFixtureCommand(t, shell, args, environment)
			if output, err = again.CombinedOutput(); err != nil {
				t.Fatalf("existing binary was not replaced: %v\n%s", err, output)
			}
			if _, err = exec.Command(binary, "version").CombinedOutput(); err != nil {
				t.Fatalf("reinstalled executable cannot run: %v", err)
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
	asset := "agentcfg_" + osName + "_" + arch + ".tar.gz"
	data := bashArchive(t)
	for _, bad := range []bool{false, true} {
		name := "valid"
		if bad {
			name = "invalid-sha256"
		}
		t.Run(name, func(t *testing.T) {
			server := fixtureServer(t, asset, data, bad)
			dir := filepath.Join(t.TempDir(), "bin")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			installed := filepath.Join(dir, "agentcfg")
			if bad {
				if err := os.WriteFile(installed, []byte("previous executable"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{filepath.Join(root(t), "install.sh")}
			environment := append(os.Environ(), "AGENTCFG_VERSION=latest", "AGENTCFG_RELEASE_BASE_URL="+server.URL, "AGENTCFG_BIN_DIR="+dir)
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
			if bad {
				if err == nil || !strings.Contains(string(output), "SHA-256 mismatch") {
					t.Fatalf("bad SHA-256 not rejected: %v\n%s", err, output)
				}
				if existing, readErr := os.ReadFile(installed); readErr != nil || string(existing) != "previous executable" {
					t.Fatalf("bad SHA-256 corrupted existing executable: %q (%v)", existing, readErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Bash installation failed: %v\n%s", err, output)
			}
			if _, err = os.Stat(installed); err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" {
				if help, err := exec.Command(installed).CombinedOutput(); err != nil || !bytes.Contains(help, []byte("Usage: agentcfg")) {
					t.Fatalf("no-argument help failed: %v\n%s", err, help)
				}
			}
			again := newCommand()
			again.Env = environment
			if output, err = again.CombinedOutput(); err != nil {
				t.Fatalf("second install could not replace binary: %v\n%s", err, output)
			}
		})
	}
}
