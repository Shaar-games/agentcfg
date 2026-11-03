package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDefaultAndUICommandsOpenTheSameChecklistDirectly(t *testing.T) {
	t.Setenv("TERM", "dumb")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, args := range [][]string{nil, {"ui"}, {"tui"}, {"harnesses"}} {
		var rendered, diagnostics bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader("0\n"), &rendered, &diagnostics); err != nil {
			t.Fatalf("agentmux %v: %v (%s)", args, err, diagnostics.String())
		}
		if !strings.Contains(rendered.String(), "Harness availability") ||
			!strings.Contains(rendered.String(), "Claude Code") ||
			!strings.Contains(rendered.String(), "Confirm selection") {
			t.Fatalf("agentmux %v did not show the checkbox list: %s", args, rendered.String())
		}
		if strings.Contains(rendered.String(), "MCP servers") ||
			strings.Contains(rendered.String(), "Exit") {
			t.Fatalf("agentmux %v unexpectedly opened the old navigation menu: %s", args, rendered.String())
		}
	}
}

func TestAdvancedMenuRemainsExplicitlyAvailable(t *testing.T) {
	t.Setenv("TERM", "dumb")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var rendered, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"menu"}, strings.NewReader("8\n"), &rendered, &diagnostics); err != nil {
		t.Fatalf("agentmux menu: %v (%s)", err, diagnostics.String())
	}
	if !strings.Contains(rendered.String(), "MCP servers") || !strings.Contains(rendered.String(), "Agent Plugins") {
		t.Fatalf("advanced menu unavailable: %s", rendered.String())
	}
}
