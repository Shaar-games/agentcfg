package tui

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestHuhAccessibleMainMenuExitIsReadOnly(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := t.TempDir()
	var rendered bytes.Buffer
	if err := Run(context.Background(), root, strings.NewReader("8\n"), &rendered); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"AgentMux", "Harnesses", "MCP servers", "Skills", "Agent Plugins", "Diagnostics", "synchronization", "Refresh", "Exit"} {
		if !strings.Contains(rendered.String(), text) {
			t.Fatalf("Huh main menu missing %q: %s", text, rendered.String())
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("opening Huh configuration wrote to user home: %v (%v)", entries, err)
	}
}

func TestHuhWindowsPathsAndControlCharactersDisplaySafely(t *testing.T) {
	value := safeHuhText("C:\\Users\\demo\\.agentmux\\skills\\review\x1b[2J\nNext")
	if value != "C:/Users/demo/.agentmux/skills/review [2J\nNext" {
		t.Fatalf("unexpected terminal-safe text: %q", value)
	}
}
