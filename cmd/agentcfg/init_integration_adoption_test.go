package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInitYesCreatesCanonicalWithoutSilentlyAdoptingNativeMCP(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a real CLI executable")
	}
	home := t.TempDir()
	nativeDir := filepath.Join(home, ".cursor")
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(nativeDir, "mcp.json")
	before := []byte(`{"otherPreference": {"keep": true}, "mcpServers": {"imported": {"command": "node", "args": ["server.js"]}}}`)
	if err := os.WriteFile(native, before, 0o600); err != nil {
		t.Fatal(err)
	}
	projectRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "agentcfg")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	build := exec.Command("go", "build", "-trimpath", "-o", executable, "./cmd/agentcfg")
	build.Dir = projectRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile CLI fixture: %v\n%s", err, output)
	}
	cmd := exec.Command(executable, "init", "--yes")
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("init --yes failed: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("Created canonical agentcfg configuration")) ||
		!bytes.Contains(output, []byte("Native harness configuration was not modified")) ||
		bytes.Contains(output, []byte("Applied")) {
		t.Fatalf("noninteractive init unexpectedly applied native changes: %s", output)
	}
	after, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("noninteractive init must not alter existing native MCP settings: %s", after)
	}
	if _, err := os.Stat(filepath.Join(home, ".agentcfg", "mcp.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".agentcfg", "sync-user.json")); !os.IsNotExist(err) {
		t.Fatalf("noninteractive init unexpectedly applied native ownership manifest: %v", err)
	}
	// A second init is not a silent overwrite or merge of existing sources.
	repeat := exec.Command(executable, "init", "--yes")
	repeat.Env = cmd.Env
	if repeatOutput, err := repeat.CombinedOutput(); err == nil ||
		!strings.Contains(string(repeatOutput), "already exists") {
		t.Fatalf("repeated init unexpectedly rewrote canonical data: %v\n%s", err, repeatOutput)
	}
}
