package syncer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/config"
)

func TestPowerShellBOMNativeJSONPreservesUnmanagedContent(t *testing.T) {
	root := t.TempDir()
	original := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"theme":"mine","mcpServers":{"personal":{"command":"keep"}}}`)...)
	path := filepath.Join(root, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Root: root, Settings: config.Settings{Harnesses: []string{"cursor"}}, Servers: map[string]config.Server{
		"demo": {Name: "demo", Type: "stdio", Command: "node", Sharing: "shared"},
	}}
	plan, err := BuildPlan(cfg, Options{Root: root, Home: root, Scope: "user", Binary: filepath.Join(root, "agentcfg.exe")})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(unchanged, original) {
		t.Fatal("preview altered native JSON")
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	edited, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(edited, []byte{0xef, 0xbb, 0xbf}) || !bytes.Contains(edited, []byte("mine")) || !bytes.Contains(edited, []byte("keep")) || !bytes.Contains(edited, []byte("demo")) {
		t.Fatalf("BOM or user settings not preserved: %q (%v)", edited, err)
	}
	again, err := BuildPlan(cfg, Options{Root: root, Home: root, Scope: "user", Binary: filepath.Join(root, "agentcfg.exe")})
	if err != nil || len(again.Changes) != 0 {
		t.Fatalf("sync not idempotent with BOM: %+v (%v)", again, err)
	}
	cfg.Settings.Harnesses = []string{}
	prune, err := BuildPlan(cfg, Options{Root: root, Home: root, Scope: "user", Binary: filepath.Join(root, "agentcfg.exe")})
	if err != nil {
		t.Fatal(err)
	}
	if err := prune.Apply(); err != nil {
		t.Fatal(err)
	}
	final, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(final, []byte{0xef, 0xbb, 0xbf}) || !strings.Contains(string(final), "personal") || strings.Contains(string(final), `"demo"`) {
		t.Fatalf("prune altered BOM/unmanaged content: %q (%v)", final, err)
	}
}

func TestPowerShellBOMCodexTOMLPreservesUserSettings(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := append([]byte{0xef, 0xbb, 0xbf}, []byte("model = 'user-model'\n[mcp_servers.personal]\ncommand = 'keep'\n")...)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Root: root, Settings: config.Settings{Harnesses: []string{"codex"}}, Servers: map[string]config.Server{
		"demo": {Name: "demo", Type: "stdio", Command: "node", Sharing: "shared"},
	}}
	opts := Options{Root: root, Home: root, Scope: "user", Binary: filepath.Join(root, "agentcfg.exe")}
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(content, original) || !bytes.Contains(content, []byte("[mcp_servers.demo]")) {
		t.Fatalf("managed section did not retain BOM/user TOML: %q (%v)", content, err)
	}
	next, err := BuildPlan(cfg, opts)
	if err != nil || len(next.Changes) != 0 {
		t.Fatalf("BOM TOML not idempotent: %+v (%v)", next, err)
	}
	cfg.Settings.Harnesses = []string{}
	removal, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := removal.Apply(); err != nil {
		t.Fatal(err)
	}
	remaining, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(remaining, original) {
		t.Fatalf("BOM TOML not restored: %q (%v)", remaining, err)
	}
}
