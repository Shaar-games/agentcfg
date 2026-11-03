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

func TestConfigOpensFullHuhMenu(t *testing.T) {
	t.Setenv("TERM", "dumb")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, args := range [][]string{{"config"}} {
		var rendered, diagnostics bytes.Buffer
		if err := run(context.Background(), args, oneByteReader{strings.NewReader("8\n0\n")}, &rendered, &diagnostics); err != nil {
			t.Fatalf("agentcfg %v: %v (%s)", args, err, diagnostics.String())
		}
		if !strings.Contains(rendered.String(), "MCP servers") ||
			!strings.Contains(rendered.String(), "Agent Plugins") ||
			!strings.Contains(rendered.String(), "Diagnostics") ||
			!strings.Contains(rendered.String(), "Confirm selection") {
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
