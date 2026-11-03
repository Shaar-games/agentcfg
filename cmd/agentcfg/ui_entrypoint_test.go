package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultIsHelpAndDoesNotAccessConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var expected string
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		var rendered, diagnostics bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader(""), &rendered, &diagnostics); err != nil {
			t.Fatalf("agentcfg %v: %v (%s)", args, err, diagnostics.String())
		}
		if expected == "" {
			expected = rendered.String()
		}
		if rendered.String() != expected {
			t.Fatalf("help differs for args %v", args)
		}
		for _, marker := range []string{"Usage: agentcfg", "config", "harnesses"} {
			if !strings.Contains(rendered.String(), marker) {
				t.Fatalf("help is missing %q", marker)
			}
		}
		if _, err := os.Lstat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
			t.Fatalf("help wrote canonical directory: %v", err)
		}
	}
}

func TestHelpHasSpacedSectionsWithoutObsoleteBoilerplate(t *testing.T) {
	var output, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"help"}, nil, &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, section := range []string{"\n\nConfiguration:\n", "\n\nSetup and inspection:\n", "\n\nRuntime:\n", "\n\nOther:\n"} {
		if !strings.Contains(text, section) {
			t.Fatalf("missing spacing or help section %q:\n%s", section, text)
		}
	}
	for _, obsolete := range []string{
		"All commands use the current user's",
		"No persistent web server or TCP listener is used.",
		"Bidirectional sync is not implemented.",
	} {
		if strings.Contains(text, obsolete) {
			t.Fatalf("obsolete help text remains: %q", obsolete)
		}
	}
}

func TestConfigOpensFullHuhMenu(t *testing.T) {
	t.Setenv("TERM", "dumb")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, args := range [][]string{{"config"}} {
		var rendered, diagnostics bytes.Buffer
		if err := run(context.Background(), args, oneByteReader{strings.NewReader("8\n")}, &rendered, &diagnostics); err != nil {
			t.Fatalf("agentcfg %v: %v (%s)", args, err, diagnostics.String())
		}
		if !strings.Contains(rendered.String(), "MCP servers") ||
			!strings.Contains(rendered.String(), "Agent Plugins") ||
			!strings.Contains(rendered.String(), "Diagnostics") ||
			!strings.Contains(rendered.String(), "Enter a number") {
			t.Fatalf("agentcfg %v did not show full config menu: %s", args, rendered.String())
		}
	}
}

func TestDeprecatedNavigationAliasesAreNotCommands(t *testing.T) {
	for _, command := range []string{"menu", "ui", "tui", "migrate"} {
		var output, diagnostics bytes.Buffer
		err := run(context.Background(), []string{command}, strings.NewReader(""), &output, &diagnostics)
		if err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("unexpected compatibility command %q: %v", command, err)
		}
	}
}

// Huh's accessible menus wrap each prompt in a fresh bufio.Reader. Deliver
// one byte per read so the first prompt cannot consume the next answer.
type oneByteReader struct{ reader *strings.Reader }

func (r oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return r.reader.Read(p[:1])
}

func TestHarnessesStillUseDirectHuhCheckboxes(t *testing.T) {
	t.Setenv("TERM", "dumb")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var rendered, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"harnesses"}, strings.NewReader("0\n"), &rendered, &diagnostics); err != nil {
		t.Fatalf("agentcfg harnesses: %v (%s)", err, diagnostics.String())
	}
	if !strings.Contains(rendered.String(), "Harness availability") || !strings.Contains(rendered.String(), "Claude Code") {
		t.Fatalf("direct harness checkbox list unavailable: %s", rendered.String())
	}
}

func TestCategoryAliasesPlainShowEffectiveHarnesses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base := filepath.Join(home, ".agentcfg")
	if err := os.MkdirAll(filepath.Join(base, "skills", "review"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(base, "agentcfg.json"):                "{\"harnesses\":[\"codex\",\"cursor\"]}",
		filepath.Join(base, "mcp.json"):                     "{\"mcpServers\":{\"review\":{\"command\":\"node\",\"harnesses\":[\"codex\"]}}}",
		filepath.Join(base, "skills", "review", "SKILL.md"): "---\nname: review\ndescription: Review code\nmetadata:\n  harness: cursor\n---\nBody unchanged\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, group := range [][]string{{"harness", "harnesses"}, {"skill", "skills"}, {"mcp", "mcps"}} {
		var expected string
		for _, command := range group {
			var output, diagnostics bytes.Buffer
			err := run(context.Background(), []string{command, "--plain"}, strings.NewReader(""), &output, &diagnostics)
			if err != nil {
				t.Fatalf("agentcfg %s --plain: %v (%s)", command, err, diagnostics.String())
			}
			if expected == "" {
				expected = output.String()
			} else if expected != output.String() {
				t.Errorf("alias %q changed output: %q != %q", command, output.String(), expected)
			}
		}
		if !strings.Contains(expected, "codex") && !strings.Contains(expected, "cursor") {
			t.Errorf("missing effective configured harnesses in: %s", expected)
		}
	}
}

func TestCompletionCommandIsReadOnlyForEveryShell(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, shell := range []string{"bash", "zsh", "fish", "powershell", "pwsh"} {
		var output, diagnostics bytes.Buffer
		err := run(context.Background(), []string{"completion", shell}, strings.NewReader(""), &output, &diagnostics)
		if err != nil {
			t.Fatalf("%s completion: %v", shell, err)
		}
		if !strings.Contains(output.String(), "agentcfg") {
			t.Fatalf("%s completion missing command", shell)
		}
	}
	if _, err := os.Lstat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
		t.Fatalf("completion created canonical configuration: %v", err)
	}
}
