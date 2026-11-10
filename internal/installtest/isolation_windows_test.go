//go:build windows

package installtest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
		relative, err := filepath.Rel(command.Dir, path)
		if !filepath.IsAbs(path) || err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Errorf("PowerShell %s escapes its temporary fixture: %q (fixture %q)", key, path, command.Dir)
		}
	}
}
