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
		for _, marker := range []string{"Usage: agentcfg", "config", "harnesses", "migrate"} {
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
	for _, args := range [][]string{{"config"}, {"menu"}, {"ui"}, {"tui"}} {
		var rendered, diagnostics bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader("8\n"), &rendered, &diagnostics); err != nil {
			t.Fatalf("agentcfg %v: %v (%s)", args, err, diagnostics.String())
		}
		if !strings.Contains(rendered.String(), "MCP servers") ||
			!strings.Contains(rendered.String(), "Agent Plugins") ||
			!strings.Contains(rendered.String(), "Diagnostics") {
			t.Fatalf("agentcfg %v did not show full config menu: %s", args, rendered.String())
		}
	}
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

func TestLegacyHomeRequiresExplicitMigration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	oldRoot := filepath.Join(home, ".agentmux")
	if err := os.Mkdir(oldRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "agentmux.json"), []byte(`{"harnesses":["codex"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var rendered, diagnostics bytes.Buffer
	err := run(context.Background(), []string{"config"}, strings.NewReader(""), &rendered, &diagnostics)
	if err == nil || !strings.Contains(err.Error(), "agentcfg migrate") {
		t.Fatalf("legacy conflict was not explained: %v", err)
	}
	if err := run(context.Background(), []string{"migrate"}, strings.NewReader(""), &rendered, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
		t.Fatalf("migration preview wrote target: %v", err)
	}
	if err := run(context.Background(), []string{"migrate", "--apply"}, strings.NewReader(""), &rendered, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(home, ".agentcfg", "agentcfg.json")); err != nil || !bytes.Contains(data, []byte("codex")) {
		t.Fatalf("legacy settings not copied: %v", err)
	}
}
