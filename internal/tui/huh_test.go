package tui

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentmux/internal/config"
)

// Huh's accessible numeric prompt constructs a new buffered reader for each
// option. Feed one byte per read to emulate a terminal instead of allowing
// one prompt to prefetch future test answers from strings.Reader.
type terminalByteReader struct{ io.Reader }

func (r terminalByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return r.Reader.Read(p[:1])
}

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

func TestHuhHarnessMultiSelectAccessible(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["claude-code","codex","cursor"]}`)
	var output bytes.Buffer
	session := interfaceSession{
		ctx: context.Background(), root: root,
		in: terminalByteReader{strings.NewReader("3\n6\n0\n\n")}, out: &output,
	}
	if err := session.configureHarnesses(); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Settings.Harnesses, []string{"claude-code", "codex", "antigravity"}) {
		t.Fatalf("accessible Huh multi-select didn't save choices: %v; output: %s", cfg.Settings.Harnesses, output.String())
	}
	if !strings.Contains(output.String(), "Confirm selection") {
		t.Fatalf("multi-select was not shown: %s", output.String())
	}
}

func TestHuhSkillMultiSelectAccessible(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root, path, _ := skillSourceFixture(t, "---\nname: review\ndescription: Skill test\nmetadata:\n  harness: codex, antigravity\n---\n# Keep\n")
	var output bytes.Buffer
	session := interfaceSession{
		ctx: context.Background(), root: root,
		in: terminalByteReader{strings.NewReader("2\n0\n\n")}, out: &output,
	}
	if err := session.configureSkill("review"); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(updated), "harness: antigravity") {
		t.Fatalf("Huh skill multi-select failed: %q (%v); output: %s", updated, err, output.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".agentmux", "backups", "skills")); err != nil {
		t.Fatal(err)
	}
}

func TestHuhWindowsPathsAndControlCharactersDisplaySafely(t *testing.T) {
	value := safeHuhText("C:\\Users\\demo\\.agentmux\\skills\\review\x1b[2J\nNext")
	if value != "C:/Users/demo/.agentmux/skills/review [2J\nNext" {
		t.Fatalf("unexpected terminal-safe text: %q", value)
	}
}
