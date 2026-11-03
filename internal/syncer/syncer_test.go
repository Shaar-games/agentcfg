package syncer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/config"
)

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) (*config.Config, Options) {
	t.Helper()
	root := t.TempDir()
	skill := filepath.Join(root, ".agentcfg", "skills", "review")
	writeTestFile(t, filepath.Join(skill, "SKILL.md"), "---\nname: review\ndescription: Review code\n---\nRead code.\n")
	writeTestFile(t, filepath.Join(skill, "references", "rules.txt"), "Preserve behavior.\n")
	cfg := &config.Config{Root: root, Servers: map[string]config.Server{
		"chrome-devtools": {Type: "stdio", Command: "unused", Sharing: "shared"},
		"context7":        {Type: "streamable-http", URL: "https://example.invalid/mcp"},
	}, Skills: []config.Skill{{Name: "review", Path: skill}}, Settings: config.Settings{Harnesses: []string{"claude-code", "codex", "cursor", "opencode", "gemini"}}}
	return cfg, Options{Root: root, Home: t.TempDir(), Binary: filepath.Join(root, "bin", "agentcfg.exe"), SkillMode: "copy"}
}

func TestDryRunThenApplyIsIdempotent(t *testing.T) {
	cfg, opts := fixture(t)
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 10 {
		t.Fatalf("want 5 configs + 5 skills, got %d", len(plan.Changes))
	}
	for _, path := range []string{".mcp.json", ".cursor", ".codex"} {
		if _, err := os.Stat(filepath.Join(cfg.Root, path)); !os.IsNotExist(err) {
			t.Fatalf("dry run wrote %s", path)
		}
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	next, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Changes) != 0 {
		t.Fatalf("not idempotent: %+v", next.Changes)
	}
	if err := next.Apply(); err != nil {
		t.Fatal(err)
	}
	var document map[string]map[string]map[string]any
	data, err := os.ReadFile(filepath.Join(cfg.Root, ".cursor", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	servers := document["mcpServers"]
	if len(servers) != 2 {
		t.Fatalf("wanted independent MCP entries: %s", data)
	}
	for name, entry := range servers {
		if entry["command"] != opts.Binary {
			t.Fatal(entry)
		}
		encoded, _ := json.Marshal(entry["args"])
		if !strings.Contains(string(encoded), `"--server","`+name+`"`) || !strings.Contains(string(encoded), `"--harness","cursor"`) {
			t.Fatal(string(encoded))
		}
	}
	if got, err := os.ReadFile(filepath.Join(cfg.Root, ".claude", "skills", "review", "references", "rules.txt")); err != nil || string(got) != "Preserve behavior.\n" {
		t.Fatalf("nested skill asset missing: %s %v", got, err)
	}
}

func TestJSONCPreservesCommentsAndUnrelatedEntries(t *testing.T) {
	cfg, opts := fixture(t)
	cfg.Settings.Harnesses = []string{"opencode"}
	path := filepath.Join(cfg.Root, "opencode.jsonc")
	writeTestFile(t, path, "{\n // keep this comment\n \"theme\": \"dark\",\n \"mcp\": {\"existing\": {\"type\": \"local\", \"command\": [\"keep-me\"]}},\n}\n")
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"keep this comment", "dark", "keep-me", "chrome-devtools", "context7"} {
		if !strings.Contains(string(data), text) {
			t.Fatalf("lost %q: %s", text, data)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, "opencode.json")); !os.IsNotExist(err) {
		t.Fatal("created conflicting opencode.json")
	}
}

func TestTOMLPreservesUnmanagedBytes(t *testing.T) {
	cfg, opts := fixture(t)
	cfg.Settings.Harnesses = []string{"codex"}
	path := filepath.Join(cfg.Root, ".codex", "config.toml")
	original := "# keep my settings\nmodel = 'custom-model'\n\n[mcp_servers.personal]\ncommand = 'do-not-touch'\n"
	writeTestFile(t, path, original)
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), original) {
		t.Fatalf("unmanaged TOML changed: %s", data)
	}
	next, err := BuildPlan(cfg, opts)
	if err != nil || len(next.Changes) != 0 {
		t.Fatalf("%v %+v", err, next)
	}
}

func TestFiltersAndPruneOnlyOwned(t *testing.T) {
	cfg, opts := fixture(t)
	cfg.Settings.Harnesses = []string{"cursor", "claude-code"}
	entry := cfg.Servers["chrome-devtools"]
	entry.Harnesses = []string{"cursor"}
	cfg.Servers["chrome-devtools"] = entry
	writeTestFile(t, filepath.Join(cfg.Root, ".mcp.json"), `{"mcpServers":{"personal":{"command":"keep"}},"other":true}`)
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(cfg.Root, ".mcp.json"))
	if strings.Contains(string(data), "chrome-devtools") {
		t.Fatal("harness filter ignored")
	}
	delete(cfg.Servers, "context7")
	cfg.Skills = nil
	plan, err = BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(cfg.Root, ".mcp.json"))
	if !strings.Contains(string(data), "personal") || !strings.Contains(string(data), "other") || strings.Contains(string(data), "context7") {
		t.Fatal(string(data))
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, ".claude", "skills", "review")); !os.IsNotExist(err) {
		t.Fatal("managed skill not pruned")
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, ".agentcfg", "skills", "review", "SKILL.md")); err != nil {
		t.Fatal("canonical source removed")
	}
}

func TestRefusesManualDriftAndNameCollisions(t *testing.T) {
	for _, scenario := range []string{"collision", "drift", "removed", "skill-drift"} {
		t.Run(scenario, func(t *testing.T) {
			cfg, opts := fixture(t)
			cfg.Settings.Harnesses = []string{"cursor"}
			path := filepath.Join(cfg.Root, ".cursor", "mcp.json")
			if scenario == "collision" {
				writeTestFile(t, path, `{"mcpServers":{"context7":{"command":"mine"}}}`)
			} else {
				plan, err := BuildPlan(cfg, opts)
				if err != nil {
					t.Fatal(err)
				}
				if err := plan.Apply(); err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "removed":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "drift":
					data, _ := os.ReadFile(path)
					writeTestFile(t, path, strings.ReplaceAll(string(data), "connect", "changed"))
				case "skill-drift":
					writeTestFile(t, filepath.Join(cfg.Root, ".cursor", "skills", "review", "manual.txt"), "keep me")
				}
			}
			if _, err := BuildPlan(cfg, opts); err == nil {
				t.Fatal("expected a conflict")
			}
		})
	}
}

func TestApplyChecksPreimagesBeforeAnyConfigWrite(t *testing.T) {
	cfg, opts := fixture(t)
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(cfg.Root, ".cursor", "mcp.json"), `{"manual":true}`)
	if err := plan.Apply(); err == nil {
		t.Fatal("expected concurrent edit conflict")
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatal("wrote a different config before detecting conflict")
	}
}

func TestSkillSourceUpdateAndUnmanagedSkillPreservation(t *testing.T) {
	cfg, opts := fixture(t)
	cfg.Settings.Harnesses = []string{"claude-code"}
	writeTestFile(t, filepath.Join(cfg.Root, ".claude", "skills", "personal", "SKILL.md"), "personal")
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(cfg.Skills[0].Path, "references", "rules.txt"), "updated")
	plan, err = BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 {
		t.Fatalf("%+v", plan.Changes)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(cfg.Root, ".claude", "skills", "review", "references", "rules.txt"))
	if string(data) != "updated" {
		t.Fatal(string(data))
	}
	data, _ = os.ReadFile(filepath.Join(cfg.Root, ".claude", "skills", "personal", "SKILL.md"))
	if string(data) != "personal" {
		t.Fatal("personal skill changed")
	}
}

func TestUserScopeUsesInjectedHomeOnly(t *testing.T) {
	cfg, opts := fixture(t)
	opts.Scope = "user"
	cfg.Settings.Harnesses = []string{"cursor"}
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range plan.Changes {
		rel, err := filepath.Rel(opts.Home, change.Path)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Fatalf("escaped fake home: %s", change.Path)
		}
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
}

func TestSymlinkTargetsAreNotWrittenThrough(t *testing.T) {
	cfg, opts := fixture(t)
	cfg.Settings.Harnesses = []string{"cursor"}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cfg.Root, ".cursor")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := BuildPlan(cfg, opts); err == nil {
		t.Fatal("followed symlink to external target")
	}
}

func TestTOMLExistingExplicitParentTable(t *testing.T) {
	cfg, opts := fixture(t)
	cfg.Settings.Harnesses = []string{"codex"}
	path := filepath.Join(cfg.Root, ".codex", "config.toml")
	original := "model = 'personal'\n[mcp_servers]\n[mcp_servers.private]\ncommand = 'keep'\n"
	writeTestFile(t, path, original)
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), original) {
		t.Fatal("unmanaged TOML changed")
	}
	next, err := BuildPlan(cfg, opts)
	if err != nil || len(next.Changes) != 0 {
		t.Fatalf("not idempotent: %v %+v", err, next)
	}
}

func TestSkillAutoLinksAndTracksSourceChanges(t *testing.T) {
	cfg, opts := fixture(t)
	cfg.Settings.Harnesses = []string{"cursor"}
	opts.SkillMode = "auto"
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(cfg.Root, ".cursor", "skills", "review")
	info, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(cfg.Skills[0].Path, "references", "rules.txt"), "changed at source")
	next, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 && len(next.Changes) != 0 {
		t.Fatalf("live symlink needlessly rewritten: %+v", next.Changes)
	}
	if err := next.Apply(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "references", "rules.txt"))
	if err != nil || string(data) != "changed at source" {
		t.Fatalf("%s %v", data, err)
	}
}

func TestPluginSkillIsNotRediscoveredAfterCodexCopy(t *testing.T) {
	root := t.TempDir()
	plugin := filepath.Join(root, ".agentcfg", "plugins", "test-plugin")
	writeTestFile(t, filepath.Join(plugin, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"test-plugin"}`)
	writeTestFile(t, filepath.Join(plugin, "skills", "review", "SKILL.md"), "---\nname: review\ndescription: Review source files\n---\nRead before editing.\n")
	writeTestFile(t, filepath.Join(root, ".agentcfg", "agentcfg.json"), `{"harnesses":["codex"]}`)
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 || len(cfg.Skills) != 1 {
		t.Fatalf("unexpected discovery: %+v", cfg)
	}
	opts := Options{Root: root, Home: t.TempDir(), Binary: filepath.Join(root, "agentcfg.exe"), SkillMode: "copy"}
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Skills) != 1 || cfg.Skills[0].Name != "test-plugin.review" {
		t.Fatalf("generated skill was rediscovered: %+v", cfg.Skills)
	}
	next, err := BuildPlan(cfg, opts)
	if err != nil || len(next.Changes) != 0 {
		t.Fatalf("not idempotent: %v %+v", err, next)
	}
}
