package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/bootstrap"
	"agentcfg/internal/config"
	"agentcfg/internal/syncer"
)

func TestInitIgnoresNativeTimingSettingsAndRestoresOriginals(t *testing.T) {
	tests := []struct {
		harness, path, document string
	}{
		{"opencode", ".config/opencode/opencode.json", `{
			"model":"keep-model","provider":{"fixture":{"options":{"apiKey":"fixture-provider-value","timeout":180000}}},
			"mcp":{
				"local":{"type":"local","command":["inert","arg"],"environment":{"MODE":"demo"},"enabled":true,"timeout":120000},
				"remote":{"type":"remote","url":"https://example.invalid/mcp","headers":{"X-Mode":"demo"},"enabled":true,"timeout":30000},
				"inactive":{"type":"local","command":["inert"],"enabled":false,"timeout":30000}
			}}`},
		{"gemini", ".gemini/settings.json", `{
			"theme":"keep-theme","mcpServers":{
				"local":{"command":"inert","args":["arg"],"env":{"MODE":"demo"},"timeout":120000},
				"remote":{"httpUrl":"https://example.invalid/mcp","headers":{"X-Mode":"demo"},"timeout":30000}
			}}`},
		{"codex", ".codex/config.toml", `model='keep-model'
[mcp_servers.local]
command='inert'
args=['arg']
startup_timeout_sec=120
tool_timeout_sec=30
[mcp_servers.local.env]
MODE='demo'
[mcp_servers.remote]
url='https://example.invalid/mcp'
startup_timeout_ms=120000
tool_timeout_sec=30.5
http_headers={X-Mode='demo'}
`},
	}
	for _, tc := range tests {
		t.Run(tc.harness, func(t *testing.T) {
			home := t.TempDir()
			native := filepath.Join(home, filepath.FromSlash(tc.path))
			writeScopeFixture(t, native, tc.document)
			inventory, err := bootstrap.Discover(home)
			if err != nil {
				t.Fatal(err)
			}
			proposal, err := proposalFromNative(inventory)
			if err != nil {
				t.Fatal(err)
			}
			count := 2
			if tc.harness == "opencode" {
				count = 3
			}
			if len(proposal.Servers) != count || len(proposal.Advisories) != count {
				t.Fatal("native timing options blocked import or were not reported")
			}
			var output bytes.Buffer
			binary := cliBinary(t)
			if err := runInitWizard(context.Background(), home, proposal, initOptions{Yes: true, Harnesses: []string{tc.harness}, Binary: binary}, nil, &output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "Warning: MCP") || strings.Contains(output.String(), "fixture-provider-value") {
				t.Fatal("missing advisory or private provider data leaked")
			}
			canonical := mustRead(t, filepath.Join(home, ".agentcfg", "mcp.json"))
			var document struct {
				Servers map[string]map[string]any `json:"mcpServers"`
			}
			if err := json.Unmarshal(canonical, &document); err != nil {
				t.Fatal(err)
			}
			for _, server := range document.Servers {
				for key := range server {
					if strings.Contains(key, "timeout") {
						t.Fatal("native-only setting was copied into canonical MCP format")
					}
				}
			}
			cfg, err := config.Load(home)
			if err != nil || len(cfg.Warnings) != 0 || len(cfg.Servers) != count {
				t.Fatalf("invalid canonical import: %v", err)
			}
			local, remote := cfg.Servers["local"], cfg.Servers["remote"]
			if local.Command != "inert" || len(local.Args) != 1 || local.Args[0] != "arg" || local.Env["MODE"] != "demo" || remote.URL != "https://example.invalid/mcp" || remote.Headers["X-Mode"] != "demo" {
				t.Fatal("connection settings changed while ignoring timing settings")
			}
			migrated := mustRead(t, native)
			if !bytes.Contains(migrated, []byte("connect")) {
				t.Fatal("imported entries were not replaced with broker proxies")
			}
			if tc.harness == "opencode" && (!bytes.Contains(migrated, []byte("fixture-provider-value")) || !bytes.Contains(migrated, []byte("180000")) || bytes.Contains(migrated, []byte("inactive")) || config.Allows(cfg.Servers["inactive"].Harnesses, "opencode")) {
				t.Fatal("provider settings changed or the disabled native upstream was not centralized while staying disabled")
			}
			plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Scope: "user", Binary: binary, SkillMode: "copy"})
			if err != nil || len(plan.Changes) != 0 {
				t.Fatalf("post-init sync is not idempotent: %v", err)
			}
			restore, err := syncer.BuildRestorePlan(home)
			if err != nil {
				t.Fatal(err)
			}
			if err := restore.Apply(); err != nil {
				t.Fatal(err)
			}
			if string(mustRead(t, native)) != tc.document {
				t.Fatal("restore did not recover original native timeouts and settings")
			}
		})
	}
}
