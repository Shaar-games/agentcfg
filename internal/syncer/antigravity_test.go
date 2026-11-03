package syncer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/config"
)

func TestAntigravityGlobalMCPAndSkills(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	skillRoot := filepath.Join(root, ".agentcfg", "skills", "review")
	writeTestFile(t, filepath.Join(skillRoot, "SKILL.md"), "---\nname: review\ndescription: Review source.\nmetadata:\n  harness: antigravity\n---\n")
	mcpPath := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	writeTestFile(t, mcpPath, "{\"mcpServers\":{\"personal\":{\"command\":\"unchanged\"}},\"otherSetting\":17}")
	cfg := &config.Config{
		Root:     root,
		Servers:  map[string]config.Server{"chrome": {Name: "chrome", Type: "stdio", Command: "node", Sharing: "shared", Harnesses: []string{"antigravity"}}},
		Skills:   []config.Skill{{Name: "review", Path: skillRoot, Harnesses: []string{"antigravity"}}},
		Settings: config.Settings{Harnesses: []string{"antigravity"}},
	}
	opts := Options{Root: root, Home: home, Binary: filepath.Join(root, "agentcfg.exe"), Scope: "user", SkillMode: "copy"}
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 2 {
		t.Fatalf("expected MCP config and skill, got %d: %+v", len(plan.Changes), plan.Changes)
	}
	if err = plan.Apply(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
		Other      int                       `json:"otherSetting"`
	}
	if err = json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if document.Other != 17 || document.MCPServers["personal"]["command"] != "unchanged" {
		t.Fatal("unmanaged config was modified", string(raw))
	}
	server := document.MCPServers["chrome"]
	if server["command"] != opts.Binary || server["type"] != nil {
		t.Fatalf("invalid Antigravity stdio entry: %#v", server)
	}
	args, ok := server["args"].([]any)
	if !ok || len(args) != 5 || args[0] != "connect" || args[2] != "chrome" || args[4] != "antigravity" {
		t.Fatalf("invalid Antigravity proxy args: %#v", server["args"])
	}
	target := filepath.Join(home, ".gemini", "config", "skills", "review", "SKILL.md")
	if _, err = os.Stat(target); err != nil {
		t.Fatal(err)
	}
	again, err := BuildPlan(cfg, opts)
	if err != nil || len(again.Changes) != 0 {
		t.Fatalf("not idempotent: %v %+v", err, again)
	}
}

func TestAntigravityPreservesUnmanagedMCPOnCollision(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(home, ".gemini", "config", "mcp_config.json"), `{"mcpServers":{"existing":{"command":"original"}}}`)
	cfg := &config.Config{
		Root: root, Servers: map[string]config.Server{"existing": {Name: "existing", Type: "stdio", Command: "other"}},
		Settings: config.Settings{Harnesses: []string{"antigravity"}},
	}
	_, err := BuildPlan(cfg, Options{Root: root, Home: home, Binary: filepath.Join(root, "agentcfg"), Scope: "user"})
	if err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("expected unmanaged collision, got %v", err)
	}
}
