package tui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func namedBackFixture(t *testing.T) (string, string) {
	t.Helper()
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["codex"]}`)
	if err := os.WriteFile(filepath.Join(root, ".agentcfg", "mcp.json"),
		[]byte(`{"mcpServers":{"back":{"command":"node"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(root, ".agentcfg", "skills", "back", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: back\ndescription: A skill named back\nmetadata:\n  harness: codex\n---\nOriginal skill body.\n"
	if err := os.WriteFile(skillPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, skillPath
}

func TestInventorySelectsResourcesNamedBackWithoutConfusingBackAction(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root, path := namedBackFixture(t)
	settingsBefore := mustRead(t, settingsFile(root))
	skillBefore := mustRead(t, path)
	var output bytes.Buffer
	// Inventory MCP -> item 'back' -> Back -> Inventory Skills -> item
	// 'back' -> Back -> Exit. No terminalByteReader prefetch workaround.
	if err := RunList(context.Background(), root, strings.NewReader("2\n1\n2\n3\n1\n2\n5\n"), &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"MCP: back", "Skill: back"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("a resource literally named back was not opened (%q): %s", want, output.String())
		}
	}
	if !bytes.Equal(settingsBefore, mustRead(t, settingsFile(root))) || !bytes.Equal(skillBefore, mustRead(t, path)) {
		t.Fatal("read-only list changed canonical settings or source SKILL.md")
	}
}

func TestConfigurationSelectsMCPAndSkillNamedBack(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root, path := namedBackFixture(t)
	var output bytes.Buffer
	// MCP 'back' -> details -> Back from MCP list.
	if err := RunMCP(context.Background(), root, strings.NewReader("1\n3\n2\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "MCP details") || !strings.Contains(output.String(), "Name: back") {
		t.Fatalf("MCP action back shadowed real MCP name: %s", output.String())
	}
	output.Reset()
	// Skill 'back' -> accept unchanged availability -> Back.
	if err := RunSkills(context.Background(), root, strings.NewReader("1\n0\n2\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Skill availability: back") {
		t.Fatalf("Back action shadowed real skill name: %s", output.String())
	}
	if !strings.Contains(string(mustRead(t, path)), "Original skill body.") {
		t.Fatal("no-op navigation rewrote skill")
	}
}

func TestNamespacedResourceValuesNeverBecomeSpecialActions(t *testing.T) {
	for _, name := range []string{"back", "exit", "refresh", "sync", "details", "item:back"} {
		value := navigationItemValue(name)
		if value == name {
			t.Fatalf("resource %q collided with an action sentinel", name)
		}
		decoded, ok := navigationItemName(value)
		if !ok || decoded != name {
			t.Fatalf("resource value was not reversible: %q -> %q", name, decoded)
		}
		if navigationColor(value, name) == navExitColor && name == "exit" {
			t.Fatal("MCP/skill item named exit must not use destructive Exit style")
		}
	}
	if _, ok := navigationItemName("back"); ok {
		t.Fatal("literal back action sentinel was mistaken for a resource")
	}
}
