package tui

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentmux/internal/config"
)

func settingsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, ".agentmux")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "mcp.json"), []byte(`{"mcpServers":{"alpha":{"command":"node","sharing":"shared"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func putSettings(t *testing.T, root string, data string) {
	t.Helper()
	if err := os.WriteFile(settingsFile(root), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessSettingsAtomicBackupAndPreservation(t *testing.T) {
	root := settingsFixture(t)
	original := `{"harnesses":["codex","antigravity"],"runtime":{"idleTimeout":"7m"},"serverOverrides":{"alpha":{"harnesses":["codex"]}}}`
	putSettings(t, root, original)
	if err := setHarnessEnabled(root, "cursor", true, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !enabledForHarness(cfg.Settings.Harnesses, "cursor") || cfg.Settings.Runtime.IdleTimeout != "7m" || len(cfg.Settings.ServerOverrides["alpha"].Harnesses) != 1 {
		t.Fatalf("settings lost other fields: %+v", cfg.Settings)
	}
	backups, err := filepath.Glob(settingsFile(root) + ".bak-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("expected one backup, got %v %v", backups, err)
	}
	data, err := os.ReadFile(backups[0])
	if err != nil || !bytes.Equal(data, []byte(original)) {
		t.Fatalf("backup not byte-identical to original: %s %v", data, err)
	}
	current, err := os.ReadFile(settingsFile(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := setHarnessEnabled(root, "cursor", false, false); err == nil || !strings.Contains(err.Error(), "modified elsewhere") {
		t.Fatalf("stale toggle should fail: %v", err)
	}
	after, err := os.ReadFile(settingsFile(root))
	if err != nil || !bytes.Equal(current, after) {
		t.Fatal("stale write modified settings")
	}
	if err := setHarnessEnabled(root, "vscode", true, false); err == nil {
		t.Fatal("unsupported user-scope VS Code was enabled")
	}
}

func TestPowerShellUTF8BOMSettingsEditAndPluginInventory(t *testing.T) {
	root := settingsFixture(t)
	original := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"harnesses":["codex"],"serverOverrides":{},"runtime":{"idleTimeout":"7m"}}`)...)
	if err := os.WriteFile(settingsFile(root), original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := setHarnessEnabled(root, "cursor", true, false); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(settingsFile(root))
	if err != nil || !bytes.HasPrefix(updated, []byte{0xef, 0xbb, 0xbf}) {
		t.Fatalf("BOM lost: %v (%q)", err, updated)
	}
	cfg, err := config.Load(root)
	if err != nil || !enabledForHarness(cfg.Settings.Harnesses, "cursor") || cfg.Settings.Runtime.IdleTimeout != "7m" {
		t.Fatalf("PowerShell settings corrupted: %+v (%v)", cfg, err)
	}
	backups, _ := filepath.Glob(settingsFile(root) + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("original backup missing: %v", backups)
	}
	backedUp, err := os.ReadFile(backups[0])
	if err != nil || !bytes.Equal(backedUp, original) {
		t.Fatal("BOM backup not byte-for-byte")
	}
	plugin := filepath.Join(root, ".agentmux", "plugins", "demo")
	if err := os.MkdirAll(plugin, 0700); err != nil {
		t.Fatal(err)
	}
	raw := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo"}`)...)
	if err := os.WriteFile(filepath.Join(plugin, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	plugins := discoverPlugins(root, cfg)
	if len(plugins) != 1 || !plugins[0].Valid || plugins[0].Name != "demo" {
		t.Fatalf("BOM plugin incorrectly reported invalid: %+v", plugins)
	}
}

func TestServerSharingOverrideKeepsExistingFilters(t *testing.T) {
	root := settingsFixture(t)
	putSettings(t, root, `{"serverOverrides":{"alpha":{"harnesses":["codex"]}},"harnesses":["codex"]}`)
	if err := setServerSharing(root, "alpha", "isolated", "shared"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["alpha"].Sharing != "isolated" || !config.Allows(cfg.Servers["alpha"].Harnesses, "codex") || config.Allows(cfg.Servers["alpha"].Harnesses, "cursor") {
		t.Fatalf("server override changed filters: %+v", cfg.Servers["alpha"])
	}
	var fields map[string]json.RawMessage
	updated, err := os.ReadFile(settingsFile(root))
	if err != nil || json.Unmarshal(updated, &fields) != nil {
		t.Fatal("settings JSON invalid")
	}
	if err := setServerSharing(root, "alpha", "shared", "shared"); err == nil {
		t.Fatal("stale sharing mode accepted")
	}
	if err := setServerSharing(root, "alpha", "private", "isolated"); err == nil {
		t.Fatal("invalid sharing mode accepted")
	}
}

func TestMissingSettingsCreatedAndInvalidSettingsPreserved(t *testing.T) {
	root := settingsFixture(t)
	if err := setHarnessEnabled(root, "codex", false, true); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil || enabledForHarness(cfg.Settings.Harnesses, "codex") {
		t.Fatalf("defaults were not used for missing settings: %v %+v", err, cfg)
	}
	bad := `{"harnesses":["unknown"]}`
	putSettings(t, root, bad)
	if err := setHarnessEnabled(root, "cursor", true, false); err == nil {
		t.Fatal("invalid settings file was overwritten")
	}
	content, err := os.ReadFile(settingsFile(root))
	if err != nil || string(content) != bad {
		t.Fatal("invalid original config was not preserved")
	}
}

func TestPluginDiscoveryIncludesEmptyPluginAndInvalidManifest(t *testing.T) {
	root := settingsFixture(t)
	base := filepath.Join(root, ".agentmux", "plugins")
	good := filepath.Join(base, "demo")
	bad := filepath.Join(base, "broken")
	for _, dir := range []string{good, bad} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	data := `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo","description":"Demo plugin"}`
	if err := os.WriteFile(filepath.Join(good, "plugin.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	plugins := discoverPlugins(root, cfg)
	if len(plugins) != 2 {
		t.Fatalf("unexpected plugins: %+v", plugins)
	}
	for _, plugin := range plugins {
		if plugin.Name == "demo" && !plugin.Valid || plugin.Name == "broken" && plugin.Valid {
			t.Fatalf("invalid plugin availability: %+v", plugin)
		}
	}
}
