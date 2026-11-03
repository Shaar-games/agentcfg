package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Windows PowerShell 5.1 writes a UTF-8 BOM for Set-Content -Encoding UTF8.
// These source documents must still load without running any processes or
// changing their original bytes.
func TestPowerShellUTF8BOMCanonicalAndPortablePlugins(t *testing.T) {
	root := t.TempDir()
	sources := map[string]string{
		".agentcfg/agentcfg.json":            `{"harnesses":["codex","antigravity"]}`,
		".agentcfg/mcp.json":                 `{"mcpServers":{"local":{"type":"stdio","command":"node"}}}`,
		".agentcfg/plugins/demo/plugin.json": `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo"}`,
		".agentcfg/plugins/demo/mcp.json":    `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{"remote":{"type":"streamable-http","url":"https://example.com/mcp"}}}`,
	}
	for name, body := range sources {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		data := append([]byte{0xef, 0xbb, 0xbf}, []byte(body)...)
		if err := os.WriteFile(target, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 || len(cfg.Settings.Harnesses) != 2 || len(cfg.Servers) != 2 {
		t.Fatalf("BOM sources were not discovered: settings=%+v servers=%v warnings=%v", cfg.Settings, cfg.Servers, cfg.Warnings)
	}
	if _, exists := cfg.Servers["demo.remote"]; !exists {
		t.Fatal("portable MCP was not discovered")
	}
	for name, body := range sources {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || string(data) != "\ufeff"+body {
			t.Fatalf("source altered: %s (%v)", name, err)
		}
	}
}
