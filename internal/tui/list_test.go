package tui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunListAccessibleExitWithoutWriting(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := t.TempDir()
	var output bytes.Buffer
	if err := RunList(context.Background(), root, strings.NewReader("6\n"), &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"agentcfg", "Inventory", "Harnesses", "MCP servers", "Skills", "Agent Plugins", "Refresh", "Exit", "Enter a number between 1 and 6"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing %q in list menu: %s", expected, output.String())
		}
	}
	if strings.Contains(output.String(), "Confirm selection") || strings.Contains(output.String(), "Select up to") {
		t.Fatal("list menu must use Huh Select for single action")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("read-only list touched root: %v %v", entries, err)
	}
}

func TestRunListMCPAndSkillsDetailsWithoutWriting(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["codex"]}`)
	skillFile := filepath.Join(root, ".agentcfg", "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o700); err != nil {
		t.Fatal(err)
	}
	skillData := []byte("---\nname: review\ndescription: Review code\nmetadata:\n  harness: codex\n---\nContent stays as it is.\n")
	if err := os.WriteFile(skillFile, skillData, 0o600); err != nil {
		t.Fatal(err)
	}
	settingsBefore, err := os.ReadFile(settingsFile(root))
	if err != nil {
		t.Fatal(err)
	}
	// Inventory -> MCP -> alpha -> Back -> Skills -> review -> Back -> Exit.
	var output bytes.Buffer
	seq := "2\n1\n2\n3\n1\n2\n6\n"
	if err := RunList(context.Background(), root, terminalByteReader{strings.NewReader(seq)}, &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"MCP: alpha", "Transport", "Skill: review", "Source:", "Back", "Available to: Codex"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing %q in read-only drilldown: %s", expected, output.String())
		}
	}
	settingsAfter, err := os.ReadFile(settingsFile(root))
	if err != nil || !bytes.Equal(settingsBefore, settingsAfter) {
		t.Fatalf("read-only list rewrote settings: %v", err)
	}
	skillAfter, err := os.ReadFile(skillFile)
	if err != nil || !bytes.Equal(skillData, skillAfter) {
		t.Fatalf("read-only list rewrote skill: %v", err)
	}
	for _, path := range []string{filepath.Join(root, ".codex"), filepath.Join(root, ".agents", "skills"), filepath.Join(root, ".agentcfg", "backups")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("read-only list created native output %s: %v", path, err)
		}
	}
}

func TestRunListHarnessesReadOnlyWithoutExtraSpacingAfterReturn(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := t.TempDir()
	var output bytes.Buffer
	// Inventory -> Harnesses -> Claude Code -> Back -> Exit.
	if err := RunList(context.Background(), root, strings.NewReader("1\n1\n8\n6\n"), &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Harness: Claude Code", "MCP config", "Skills", "Back"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("harness list missing %q: %s", expected, output.String())
		}
	}
	if strings.Contains(output.String(), "\n\n\n") {
		t.Fatalf("menu transitions added blank lines: %q", output.String())
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("list modified home fixture: %v %v", entries, err)
	}
}
