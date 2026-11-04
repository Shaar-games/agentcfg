package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"agentcfg/internal/bootstrap"
	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"
	"agentcfg/internal/syncer"
	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
)

type duplicateMCPSource struct{ harness, path, body string }

func TestInitDuplicateMCPsRoundTrip(t *testing.T) {
	cursor := duplicateMCPSource{"cursor", ".cursor/mcp.json", `{"preference":"keep","mcpServers":{"browsermcp":{"command":"inert","args":[]}}}`}
	opencode := duplicateMCPSource{"opencode", ".config/opencode/opencode.json", `{"preference":"keep","mcp":{"browsermcp":{"type":"local","command":["inert"]}}}`}
	codex := duplicateMCPSource{"codex", ".codex/config.toml", "model='keep'\n[mcp_servers.browsermcp]\ncommand='inert'\nargs=[]\n"}
	tests := []struct {
		name    string
		sources []duplicateMCPSource
		count   int
	}{
		{"identical-cross-harness", []duplicateMCPSource{cursor, opencode}, 1},
		{"all-harnesses", []duplicateMCPSource{cursor, opencode, codex,
			{"claude-code", ".claude.json", `{"mcpServers":{"browsermcp":{"command":"inert"}}}`},
			{"gemini", ".gemini/settings.json", `{"mcpServers":{"browsermcp":{"command":"inert"}}}`},
			{"antigravity", ".gemini/config/mcp_config.json", `{"mcpServers":{"browsermcp":{"command":"inert"}}}`}}, 1},
		{"four-opencode-files", []duplicateMCPSource{opencode,
			{"opencode", ".config/opencode/opencode.jsonc", "// Keep this comment.\n" + opencode.body},
			{"opencode", ".opencode/opencode.json", opencode.body},
			{"opencode", ".opencode/opencode.jsonc", opencode.body}}, 1},
		{"distinct-cross-harness", []duplicateMCPSource{cursor,
			{"opencode", opencode.path, `{"mcp":{"browsermcp":{"type":"local","command":["other"]}}}`}}, 2},
		{"distinct-same-harness", []duplicateMCPSource{opencode,
			{"opencode", ".opencode/opencode.jsonc", `{"mcp":{"browsermcp":{"type":"local","command":["other"]}}}`}}, 2},
		{"disabled-other-harness", []duplicateMCPSource{cursor,
			{"opencode", opencode.path, `{"mcp":{"browsermcp":{"type":"local","command":["inert"],"enabled":false}}}`}}, 1},
		{"disabled-toml", []duplicateMCPSource{cursor,
			{"codex", codex.path, codex.body + "enabled=false\n"}}, 1},
		{"disabled-only", []duplicateMCPSource{
			{"opencode", opencode.path, `{"mcp":{"browsermcp":{"type":"local","command":["inert"],"enabled":false}}}`}}, 1},
		{"disabled-list", []duplicateMCPSource{opencode,
			{"cursor", cursor.path, `{"disabledMcpServers":["browsermcp","unrelated"],"mcpServers":{"browsermcp":{"command":"inert"}}}`}}, 1},
		{"disabled-placeholder", []duplicateMCPSource{cursor,
			{"opencode", opencode.path, `{"mcp":{"browsermcp":{"enabled":false}}}`}}, 1},
		{"disable-policy-without-entry", []duplicateMCPSource{opencode,
			{"cursor", cursor.path, `{"disabledMcpServers":["browsermcp","unrelated"],"mcpServers":{}}`}}, 1},
		{"disable-policy-without-section", []duplicateMCPSource{opencode,
			{"cursor", cursor.path, `{"disabledMcpServers":["browsermcp","unrelated"],"preference":"keep"}`}}, 1},
		{"active-and-disabled-same-harness", []duplicateMCPSource{opencode,
			{"opencode", ".opencode/opencode.jsonc", `{"mcp":{"browsermcp":{"type":"local","command":["inert"],"enabled":false}}}`}}, 1},
		{"disabled-and-distinct-multiple-files", []duplicateMCPSource{cursor,
			{"opencode", opencode.path, `{"mcp":{"browsermcp":{"type":"local","command":["inert"],"enabled":false}}}`},
			{"opencode", ".opencode/opencode.jsonc", `{"mcp":{"browsermcp":{"type":"local","command":["other"]}}}`}}, 2},
		{"disabled-distinct-only", []duplicateMCPSource{
			{"opencode", opencode.path, `{"mcp":{"browsermcp":{"type":"local","command":["inert"],"enabled":false}}}`},
			{"opencode", ".opencode/opencode.jsonc", `{"mcp":{"browsermcp":{"type":"local","command":["other"],"enabled":false}}}`}}, 2},
		{"reserved-name", []duplicateMCPSource{
			{"cursor", cursor.path, `{"mcpServers":{"browsermcp":{"command":"inert"},"browsermcp-cursor":{"command":"third"}}}`},
			{"opencode", opencode.path, `{"mcp":{"browsermcp":{"type":"local","command":["other"]}}}`}}, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			var harnesses []string
			for _, source := range tc.sources {
				writeScopeFixture(t, filepath.Join(home, filepath.FromSlash(source.path)), source.body)
				if !slices.Contains(harnesses, source.harness) {
					harnesses = append(harnesses, source.harness)
				}
			}
			inventory, err := bootstrap.Discover(home)
			if err != nil {
				t.Fatal(err)
			}
			proposal, err := proposalFromNative(inventory)
			if err != nil || len(proposal.Servers) != tc.count {
				t.Fatalf("lost duplicate definitions: %v %+v", err, inventory.Servers)
			}
			binary := cliBinary(t)
			if err := runInitWizard(context.Background(), home, proposal, initOptions{Yes: true, Harnesses: harnesses, Binary: binary}, nil, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(home)
			if err != nil || len(cfg.Warnings) != 0 || len(cfg.Servers) != tc.count {
				t.Fatalf("invalid canonical sources: %v %+v", err, cfg)
			}
			if tc.name == "disabled-only" && config.Allows(cfg.Servers["browsermcp"].Harnesses, "opencode") {
				t.Fatal("disabled MCP was enabled")
			}
			for _, source := range tc.sources {
				path := filepath.Join(home, filepath.FromSlash(source.path))
				data := mustRead(t, path)
				var document map[string]any
				if source.harness == "codex" {
					err = toml.Unmarshal(data, &document)
				} else {
					standard, standardErr := hujson.Standardize(data)
					if standardErr != nil {
						t.Fatal(standardErr)
					}
					err = json.Unmarshal(standard, &document)
				}
				if err != nil {
					t.Fatal(err)
				}
				key := "mcpServers"
				if source.harness == "codex" {
					key = "mcp_servers"
				} else if source.harness == "opencode" {
					key = "mcp"
				}
				entries, _ := document[key].(map[string]any)
				for name, server := range cfg.Servers {
					entry, exists := entries[name].(map[string]any)
					if !config.Allows(server.Harnesses, source.harness) {
						if exists {
							t.Fatalf("disabled native upstream remains: %s/%s", source.path, name)
						}
						continue
					}
					if !exists {
						t.Fatalf("missing broker proxy %s/%s", source.path, name)
					}
					if source.harness == "opencode" {
						args, ok := entry["command"].([]any)
						if !ok || len(args) < 2 || args[0] != binary || args[1] != "connect" {
							t.Fatal("OpenCode retains a direct upstream")
						}
					} else if entry["command"] != binary {
						t.Fatal("native harness retains a direct upstream")
					}
				}
				if preference, exists := document["preference"]; exists && preference != "keep" {
					t.Fatal("unrelated preference changed")
				}
				if tc.name == "disabled-list" && source.harness == "cursor" {
					disabled, _ := document["disabledMcpServers"].([]any)
					if len(disabled) != 1 || disabled[0] != "unrelated" {
						t.Fatal("adopted disable policy was not migrated precisely")
					}
				}
			}
			plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Scope: "user", Binary: binary, SkillMode: "copy"})
			if err != nil || len(plan.Changes) != 0 {
				t.Fatalf("sync is not idempotent: %v", err)
			}
			restore, err := syncer.BuildRestorePlan(home)
			if err != nil {
				t.Fatal(err)
			}
			if err := restore.Apply(); err != nil {
				t.Fatal(err)
			}
			for _, source := range tc.sources {
				if string(mustRead(t, filepath.Join(home, filepath.FromSlash(source.path)))) != source.body {
					t.Fatalf("restore lost original definition: %s", source.path)
				}
			}
			if _, err := os.Stat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
				t.Fatal("restore did not archive canonical sources")
			}
		})
	}
}

func TestInitDuplicateMCPsWithLinkedSecondaryFile(t *testing.T) {
	for _, mode := range []string{"absolute-file", "relative-file", "relative-directory"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			original := `{"preference":"keep","mcp":{"browsermcp":{"type":"local","command":["inert"]}}}`
			primary := filepath.Join(home, ".config", "opencode", "opencode.json")
			writeScopeFixture(t, primary, original)
			alias := filepath.Join(home, ".opencode", "opencode.jsonc")
			target := primary
			if mode == "relative-directory" {
				alias = filepath.Dir(alias)
				target = filepath.Dir(primary)
			}
			if err := os.MkdirAll(filepath.Dir(alias), 0700); err != nil {
				t.Fatal(err)
			}
			if mode != "absolute-file" {
				var err error
				target, err = filepath.Rel(filepath.Dir(alias), target)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, alias); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			inventory, err := bootstrap.Discover(home)
			if err != nil {
				t.Fatal(err)
			}
			proposal, err := proposalFromNative(inventory)
			if err != nil || len(proposal.Servers) != 1 {
				t.Fatalf("linked duplicates not merged: %v", err)
			}
			binary := cliBinary(t)
			if err := runInitWizard(context.Background(), home, proposal, initOptions{Yes: true, Harnesses: []string{"opencode"}, Binary: binary}, nil, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(alias)
			if err != nil || pathutil.IsRedirected(info) {
				t.Fatalf("secondary alias was not materialized: %v", err)
			}
			cfg, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Scope: "user", Binary: binary, SkillMode: "copy"})
			if err != nil || len(plan.Changes) != 0 {
				t.Fatalf("linked duplicate sync is not idempotent: %v", err)
			}
			restore, err := syncer.BuildRestorePlan(home)
			if err != nil {
				t.Fatal(err)
			}
			if err := restore.Apply(); err != nil {
				t.Fatal(err)
			}
			if actual, err := os.Readlink(alias); err != nil || actual != target {
				t.Fatalf("secondary link not restored exactly: %v", err)
			}
			if string(mustRead(t, primary)) != original {
				t.Fatal("primary source changed after restore")
			}
		})
	}
}

func TestDuplicateMCPAdoptionRejectsEditedSecondarySource(t *testing.T) {
	home := t.TempDir()
	original := `{"mcp":{"browsermcp":{"type":"local","command":["inert"]}}}`
	primary := filepath.Join(home, ".config", "opencode", "opencode.json")
	secondary := filepath.Join(home, ".opencode", "opencode.jsonc")
	for _, path := range []string{primary, secondary} {
		writeScopeFixture(t, path, original)
	}
	inventory, err := bootstrap.Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := proposalFromNative(inventory)
	if err != nil {
		t.Fatal(err)
	}
	harnesses := []string{"opencode"}
	if err := writeInitialSources(home, config.Settings{Harnesses: harnesses}, proposal.Servers, nil); err != nil {
		t.Fatal(err)
	}
	proofs, _, err := initAdoptionProofs(inventory, proposal.Servers, nil, harnesses)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Scope: "user", Binary: filepath.Join(home, "agentcfg"), SkillMode: "copy", AdoptExisting: true, AdoptMCP: proofs})
	if err != nil {
		t.Fatal(err)
	}
	edited := `{"mcp":{"browsermcp":{"type":"local","command":["keep-my-edit"]}}}`
	writeScopeFixture(t, secondary, edited)
	if err := plan.Apply(); err == nil {
		t.Fatal("edited secondary file was overwritten")
	}
	if string(mustRead(t, primary)) != original || string(mustRead(t, secondary)) != edited {
		t.Fatal("failed adoption did not preserve both original files")
	}
}
