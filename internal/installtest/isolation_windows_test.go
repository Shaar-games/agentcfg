//go:build windows

package installtest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/pathutil"

	"golang.org/x/sys/windows"
)

func powerShellFixtureContains(fixture, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	fixture, err := pathutil.Resolve(fixture)
	if err != nil {
		return false
	}
	// PowerShell can expand an NTFS short alias, and the cache file need not
	// exist yet. Resolve its existing parents before comparing containment.
	path, err = pathutil.ResolvePotential(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(fixture, path)
	return err == nil && !filepath.IsAbs(relative) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func TestPowerShellFixtureUsesTemporaryProfileAndCache(t *testing.T) {
	shell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe is unavailable")
	}
	inherited := t.TempDir()
	t.Setenv("PSModuleAnalysisCachePath", filepath.Join(inherited, "foreign-cache"))
	t.Setenv("PSModulePath", filepath.Join(filepath.Dir(shell), "Modules"))
	const script = `$ErrorActionPreference = 'Stop'
Get-Command Invoke-WebRequest | Out-Null
@{
    Home = $HOME
    Profile = $env:USERPROFILE
    AppData = $env:APPDATA
    LocalAppData = [Environment]::GetFolderPath([Environment+SpecialFolder]::LocalApplicationData)
    Temporary = [IO.Path]::GetTempPath()
    ModuleCache = $env:PSModuleAnalysisCachePath
    WorkingDirectory = [Environment]::CurrentDirectory
} | ConvertTo-Json -Compress`
	command := powerShellFixtureCommand(t, shell, []string{"-NoProfile", "-NonInteractive", "-Command", script}, os.Environ())
	if command.Dir == "" {
		// Keep a regression in the old helper from writing into the repository.
		command.Dir = t.TempDir()
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell fixture probe: %v\n%s", err, output)
	}
	var locations map[string]string
	if err := json.Unmarshal(output, &locations); err != nil {
		t.Fatalf("PowerShell fixture locations: %v\n%s", err, output)
	}
	for _, key := range []string{"Home", "Profile", "AppData", "LocalAppData", "Temporary", "ModuleCache", "WorkingDirectory"} {
		path := locations[key]
		if !powerShellFixtureContains(command.Dir, path) {
			t.Errorf("PowerShell %s escapes its temporary fixture: %q (fixture %q)", key, path, command.Dir)
		}
	}
}

func TestPowerShellFixtureContainmentRejectsOutsidePaths(t *testing.T) {
	base := t.TempDir()
	fixture := filepath.Join(base, "profile")
	outside := filepath.Join(base, "profile-outside")
	for _, directory := range []string{filepath.Join(fixture, "cache"), outside} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name, path string
		want       bool
	}{
		{"root", fixture, true},
		{"existing-child", filepath.Join(fixture, "cache"), true},
		{"missing-cache", filepath.Join(fixture, "cache", "ModuleAnalysisCache"), true},
		{"outside", outside, false},
		{"missing-outside", filepath.Join(outside, "ModuleAnalysisCache"), false},
		{"relative", "cache", false},
		{"empty", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := powerShellFixtureContains(fixture, test.path); got != test.want {
				t.Fatalf("fixture containment for %q = %t, want %t", test.path, got, test.want)
			}
		})
	}
}

func TestPowerShellFixtureContainmentAcceptsNTFSShortAliases(t *testing.T) {
	base, err := pathutil.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(base, "native-profile-with-long-name")
	outside := long + "-outside"
	for _, directory := range []string{filepath.Join(long, "cache"), outside} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	name, err := windows.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32768)
	length, err := windows.GetShortPathName(name, &buffer[0], uint32(len(buffer)))
	if err != nil || length == 0 || length >= uint32(len(buffer)) {
		t.Skipf("NTFS short aliases unavailable: %v", err)
	}
	short := windows.UTF16ToString(buffer[:length])
	if strings.EqualFold(short, long) {
		t.Skip("filesystem did not assign a distinct 8.3 path")
	}
	t.Logf("same fixture through NTFS aliases: short=%q long=%q", short, long)
	for _, test := range []struct {
		name, fixture, path string
		want                bool
	}{
		{"short-root-long-root", short, long, true},
		{"short-root-long-child", short, filepath.Join(long, "cache"), true},
		{"long-root-short-child", long, filepath.Join(short, "cache"), true},
		{"missing-cache", short, filepath.Join(long, "cache", "ModuleAnalysisCache"), true},
		{"outside", short, outside, false},
		{"missing-outside", short, filepath.Join(outside, "ModuleAnalysisCache"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := powerShellFixtureContains(test.fixture, test.path); got != test.want {
				t.Fatalf("fixture containment for %q under %q = %t, want %t", test.path, test.fixture, got, test.want)
			}
		})
	}
}
