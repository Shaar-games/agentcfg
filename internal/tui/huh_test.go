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

	"agentcfg/internal/config"

	"github.com/charmbracelet/huh"
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

func TestHuhAccessibleCompactMenuExitIsReadOnly(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := t.TempDir()
	var rendered bytes.Buffer
	if err := RunMenu(context.Background(), root, strings.NewReader("6\n"), &rendered); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"agentcfg", "Harnesses", "MCP servers", "Skills", "Agent Plugins", "Refresh", "Exit"} {
		if !strings.Contains(rendered.String(), text) {
			t.Fatalf("Huh main menu missing %q: %s", text, rendered.String())
		}
	}
	for _, obsolete := range []string{"Diagnostics", "Synchronize", "enable / disable", " configured", " available", " folders", "warning(s)"} {
		if strings.Contains(rendered.String(), obsolete) {
			t.Errorf("compact menu still contains %q: %s", obsolete, rendered.String())
		}
	}
	if !strings.Contains(rendered.String(), "Enter a number between 1 and 6") || strings.Contains(rendered.String(), "Confirm selection") {
		t.Fatalf("configuration navigation must be a single-choice Huh Select: %s", rendered.String())
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("opening Huh configuration wrote to user home: %v (%v)", entries, err)
	}
}

func TestHuhConfigMenuOpensNativeHarnessCheckboxes(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["codex","cursor"]}`)
	before, err := os.ReadFile(settingsFile(root))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	// Enter Harnesses directly, accept prechecked values, then select Exit.
	in := strings.NewReader("1\n0\n6\n")
	if err := RunMenu(context.Background(), root, in, &output); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"agentcfg", "Harness availability", "Claude Code", "Codex", "Confirm selection"} {
		if !strings.Contains(output.String(), marker) {
			t.Fatalf("missing menu or harness checkbox content %q: %s", marker, output.String())
		}
	}
	after, err := os.ReadFile(settingsFile(root))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("unchanged checklist rewrote settings: %s", after)
	}
}

func TestHuhMCPSharingUsesPrecheckedNativeList(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["codex"]}`)
	source := filepath.Join(root, ".agentcfg", "mcp.json")
	if err := os.WriteFile(source, []byte(`{"mcpServers":{"local":{"command":"node","sharing":"shared"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	// Open server -> Change sharing -> Isolated -> Back. Each action selection
	// now completes on Enter; no checkbox confirmation is needed.
	sequence := "1\n2\n2\ny\n2\n"
	session := interfaceSession{ctx: context.Background(), root: root, in: terminalByteReader{strings.NewReader(sequence)}, out: &output}
	if err := session.configureMCP(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "MCP sharing") || strings.Contains(output.String(), "Confirm selection") {
		t.Fatalf("MCP sharing must use a single-choice Huh Select: %s", output.String())
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["local"].Sharing != "isolated" {
		t.Fatalf("new MCP sharing mode was not saved: %+v", cfg.Servers["local"])
	}
}

func TestHuhMCPAvailabilityListCanDisableOneConfiguredHarness(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := settingsFixture(t)
	putSettings(t, root, "{\"harnesses\":[\"codex\",\"cursor\"]}")
	var output bytes.Buffer
	// Open alpha, open the actual multiselect availability field, uncheck
	// Codex, confirm, then Back to the parent screen.
	input := terminalByteReader{strings.NewReader("1\n1\n1\n0\ny\n2\n")}
	session := interfaceSession{ctx: context.Background(), root: root, in: input, out: &output}
	if err := session.configureMCP(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "MCP availability: alpha") || !strings.Contains(output.String(), "Confirm selection") {
		t.Fatalf("MCP routing did not use the native Huh checkbox field: %s", output.String())
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	server := cfg.Servers["alpha"]
	if config.Allows(server.Harnesses, "codex") || !config.Allows(server.Harnesses, "cursor") {
		t.Fatalf("Huh MCP availability did not save configured harness routing: %+v; output: %s", server.Harnesses, output.String())
	}
}

func TestHuhDefaultOpensCheckboxListWithoutMenu(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["codex","cursor"]}`)
	before, err := os.ReadFile(settingsFile(root))
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	if err := Run(context.Background(), root, terminalByteReader{strings.NewReader("0\n")}, &rendered); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Harness availability", "Claude Code", "Codex", "Cursor", "Confirm selection"} {
		if !strings.Contains(rendered.String(), label) {
			t.Fatalf("missing checkbox content %q: %s", label, rendered.String())
		}
	}
	for _, unexpected := range []string{"MCP servers", "Exit", "Back to main menu"} {
		if strings.Contains(rendered.String(), unexpected) {
			t.Fatalf("unexpected intermediate menu %q: %s", unexpected, rendered.String())
		}
	}
	after, err := os.ReadFile(settingsFile(root))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("unchanged checkbox list unexpectedly rewrote configuration: %s", after)
	}
}

func TestHuhDefaultCheckboxListSavesSelection(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["codex"]}`)
	var output bytes.Buffer
	if err := Run(context.Background(), root, terminalByteReader{strings.NewReader("2\n6\n0\ny\n")}, &output); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Settings.Harnesses, []string{"antigravity"}) {
		t.Fatalf("unexpected checklist result %v: %s", cfg.Settings.Harnesses, output.String())
	}
}

func TestHuhHarnessMultiSelectAccessible(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["claude-code","codex","cursor"]}`)
	var output bytes.Buffer
	session := interfaceSession{
		ctx: context.Background(), root: root,
		in: terminalByteReader{strings.NewReader("3\n6\n0\ny\n")}, out: &output,
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

func TestHuhHarnessMultiSelectAllowsEmptySelection(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["codex"]}`)
	var output bytes.Buffer
	session := interfaceSession{
		ctx: context.Background(), root: root,
		in: terminalByteReader{strings.NewReader("2\n0\ny\n")}, out: &output,
	}
	if err := session.configureHarnesses(); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Settings.Harnesses == nil || len(cfg.Settings.Harnesses) != 0 {
		t.Fatalf("empty Huh selection must disable every harness: %v; output: %s", cfg.Settings.Harnesses, output.String())
	}
}

func TestHuhHarnessMultiSelectAcceptsPreselectedValuesUnchanged(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["codex","cursor"]}`)
	before, err := os.ReadFile(settingsFile(root))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	session := interfaceSession{
		ctx: context.Background(), root: root,
		in: terminalByteReader{strings.NewReader("0\n")}, out: &output,
	}
	if err := session.configureHarnesses(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(settingsFile(root))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("accepting default selections rewrote config: before=%s after=%s", before, after)
	}
}

func TestHuhHarnessMultiSelectAccessibleOptIn(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("AGENTCFG_ACCESSIBLE", "1")
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["codex"]}`)
	var output bytes.Buffer
	session := interfaceSession{
		ctx: context.Background(), root: root,
		in: terminalByteReader{strings.NewReader("0\n")}, out: &output,
	}
	if err := session.configureHarnesses(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Select up to 6 options") {
		t.Fatalf("explicit accessible opt-in did not render the numbered selector: %s", output.String())
	}
}

func TestHuhSkillMultiSelectAccessible(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root, path, _ := skillSourceFixture(t, "---\nname: review\ndescription: Skill test\nmetadata:\n  harness: codex, antigravity\n---\n# Keep\n")
	var output bytes.Buffer
	session := interfaceSession{
		ctx: context.Background(), root: root,
		in: terminalByteReader{strings.NewReader("1\n2\n3\n0\ny\n")}, out: &output,
	}
	if err := session.configureSkill("review"); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(updated), "harness: cursor") {
		t.Fatalf("Huh skill multi-select failed: %q (%v); output: %s", updated, err, output.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".agentcfg", "backups", "skills")); err != nil {
		t.Fatal(err)
	}
}

func TestHuhWindowsPathsAndControlCharactersDisplaySafely(t *testing.T) {
	value := safeHuhText("C:\\Users\\demo\\.agentcfg\\skills\\review\x1b[2J\nNext")
	if value != "C:/Users/demo/.agentcfg/skills/review [2J\nNext" {
		t.Fatalf("unexpected terminal-safe text: %q", value)
	}
}

func TestHuhSelectActionEnterOpensSingleChoiceWithoutExtraSpacing(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var output bytes.Buffer
	s := interfaceSession{
		ctx: context.Background(), in: terminalByteReader{strings.NewReader("2\n")}, out: &output,
	}
	selected, err := s.selectAction("Menu", "Enter opens the selected action",
		huh.NewOption("First", "first"), huh.NewOption("Second", "second"))
	if err != nil || selected != "second" {
		t.Fatalf("Select did not trigger selected action on Enter: %q (%v)", selected, err)
	}
	if strings.Contains(output.String(), "Confirm selection") || strings.Contains(output.String(), "Select up to") {
		t.Fatalf("single action rendered a checkbox workflow: %q", output.String())
	}
	if strings.HasSuffix(output.String(), "\n\n") {
		t.Fatalf("menu added unwanted blank lines: %q", output.String())
	}
	if _, err := s.selectAction("Empty", "", nil...); err == nil {
		t.Fatal("empty action list must fail explicitly instead of panicking")
	}
}

func TestHuhMCPSharingSelectUnchangedByEnter(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["codex"]}`)
	before, err := os.ReadFile(settingsFile(root))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	s := interfaceSession{ctx: context.Background(), root: root,
		in: terminalByteReader{strings.NewReader("1\n")}, out: &output}
	if err := s.configureMCPSharing("alpha", config.Server{Sharing: "shared"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(settingsFile(root))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("same sharing mode should not rewrite settings: %v; before=%s after=%s", err, before, after)
	}
}
