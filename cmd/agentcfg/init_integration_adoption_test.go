package main

import (
	"agentcfg/internal/syncer"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitYesMigratesNativeMCPAndRestoresOriginal(t *testing.T) {
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
	executable := cliBinary(t)
	cmd := exec.Command(executable, "init", "--yes")
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home,
		"LOCALAPPDATA="+filepath.Join(home, "cache"), "XDG_CACHE_HOME="+filepath.Join(home, "cache"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("init --yes failed: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("Initialized:")) {
		t.Fatalf("init did not finish migration: %s", output)
	}
	var document struct {
		Other   map[string]any `json:"otherPreference"`
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mustRead(t, native), &document); err != nil {
		t.Fatal(err)
	}
	if document.Servers["imported"].Command != executable || len(document.Servers["imported"].Args) == 0 || document.Servers["imported"].Args[0] != "connect" || document.Other["keep"] != true {
		t.Fatal("native MCP was not replaced with the agentcfg proxy")
	}
	if _, err := os.Stat(filepath.Join(home, ".agentcfg", "sync-user.json")); err != nil {
		t.Fatal(err)
	}

	// A second init is not a silent overwrite or merge of existing sources.
	repeat := exec.Command(executable, "init", "--yes")
	repeat.Env = cmd.Env
	if repeatOutput, err := repeat.CombinedOutput(); err == nil ||
		!strings.Contains(string(repeatOutput), "already exists") {
		t.Fatalf("repeated init unexpectedly rewrote canonical data: %v\n%s", err, repeatOutput)
	}
	restore, err := syncer.BuildRestorePlan(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, native), before) {
		t.Fatal("native original was not restored exactly")
	}

}
