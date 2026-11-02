package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentmux/internal/broker"
	"agentmux/internal/config"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func exampleConfig() *config.Config {
	return &config.Config{
		Root: "fixture", Settings: config.Settings{Harnesses: []string{"codex", "antigravity"}},
		Servers: map[string]config.Server{
			"one": {Name: "one", Type: "stdio", Command: "node", Sharing: "shared", Harnesses: []string{"codex"}},
			"two": {Name: "two", Type: "streamable-http", URL: "https://example.org/mcp?secret=ignored", Sharing: "isolated"},
		},
		Skills:   []config.Skill{{Name: "review", Path: "fixture/.agentmux/skills/review", Harnesses: []string{"codex"}}},
		Warnings: []string{"fixture: plugin warning"},
	}
}

func modelFixture(cfg *config.Config) Model {
	return newModel(context.Background(), "fixture", services{
		load: func(string) (*config.Config, error) { return cfg, nil },
		status: func(context.Context, string) (broker.Status, error) {
			return broker.Status{Running: true, PID: 55, Backends: []broker.BackendStatus{{Name: "one", PID: 42, Clients: 2}}}, nil
		},
		listPlugins: func(string, *config.Config) []pluginInfo {
			return []pluginInfo{{Name: "demo", Valid: true, MCP: 1, Skills: 1}}
		},
	})
}

func press(t *testing.T, m Model, key string) (Model, tea.Cmd) {
	t.Helper()
	var msg tea.KeyMsg
	switch key {
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "right":
		msg = tea.KeyMsg{Type: tea.KeyRight}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	result, cmd := m.Update(msg)
	return result.(Model), cmd
}

func TestNavigationRenderingAndResize(t *testing.T) {
	cfg := exampleConfig()
	m := modelFixture(cfg)
	result, _ := m.Update(loadedMsg{config: cfg, status: broker.Status{Running: true, PID: 55}})
	m = result.(Model)
	result, _ = m.Update(tea.WindowSizeMsg{Width: 62, Height: 22})
	m = result.(Model)
	for _, sectionKey := range []string{"1", "2", "3", "4", "5"} {
		m, _ = press(t, m, sectionKey)
		view := m.View()
		if len(strings.Split(view, "\n")) != m.height {
			t.Errorf("tab %s rendered %d lines, expected %d", sectionKey, len(strings.Split(view, "\n")), m.height)
		}
		for _, line := range strings.Split(view, "\n") {
			if lipgloss.Width(line) > m.width {
				t.Errorf("tab %s overflowed width (%d > %d): %q", sectionKey, lipgloss.Width(line), m.width, line)
			}
		}
	}
	result, _ = m.Update(tea.WindowSizeMsg{Width: 38, Height: 13})
	m = result.(Model)
	for _, line := range strings.Split(m.View(), "\n") {
		if lipgloss.Width(line) > m.width {
			t.Errorf("small window overflowed terminal width: %q", line)
		}
	}
	result, _ = m.Update(tea.WindowSizeMsg{Width: 62, Height: 22})
	m = result.(Model)
	m, _ = press(t, m, "2")
	m, _ = press(t, m, "down")
	if m.selected[mcpSection] != 1 || !strings.Contains(m.View(), "https://example.org") {
		t.Fatalf("selected MCP details unavailable: %s", m.View())
	}
	if strings.Contains(m.View(), "secret=ignored") {
		t.Fatal("remote URL query leaked in diagnostics")
	}
	m, _ = press(t, m, "tab")
	if m.section != skillSection || !strings.Contains(m.View(), "review") {
		t.Fatal("section switching lost skill inventory")
	}
	m, _ = press(t, m, " ")
	if !m.skillModal.open || !strings.Contains(m.View(), "SKILL ACCESS") {
		t.Fatal("skill dialog was not opened")
	}
	for _, line := range strings.Split(m.View(), "\n") {
		if lipgloss.Width(line) > m.width {
			t.Errorf("skill dialog overflowed terminal width: %q", line)
		}
	}
	m, _ = press(t, m, "esc")
	if m.skillModal.open {
		t.Fatal("Escape did not close skill dialog")
	}
}

func TestSkillAccessModalNavigationAndSave(t *testing.T) {
	cfg := exampleConfig()
	m := modelFixture(cfg)
	m.service.editSkill = func(root string, skill config.Skill, harness string, enabled bool) error {
		if root != "fixture" || skill.Name != "review" || harness != "antigravity" || !enabled {
			t.Fatalf("unexpected edit arguments %s %+v %s %v", root, skill, harness, enabled)
		}
		return nil
	}
	result, _ := m.Update(loadedMsg{config: cfg})
	m = result.(Model)
	m, _ = press(t, m, "3")
	m, _ = press(t, m, "a")
	if !m.skillModal.open || len(m.skillModal.options) != 2 {
		t.Fatalf("modal should enumerate enabled harnesses: %+v", m.skillModal)
	}
	// codex and antigravity are selected; codex is allowed in this skill.
	if !strings.Contains(m.View(), "[✓] Codex") || !strings.Contains(m.View(), "[ ] Antigravity") {
		t.Errorf("availability indicators are wrong:\n%s", m.View())
	}
	m, _ = press(t, m, "down")
	if m.skillModal.selected != 1 {
		t.Fatal("modal down failed")
	}
	_, cmd := press(t, m, "tab")
	if cmd != nil || m.section != skillSection {
		t.Fatal("modal keyboard must not switch sections")
	}
	m, cmd = press(t, m, " ")
	if cmd == nil || !m.busy {
		t.Fatal("space did not begin skill toggle")
	}
	_, ignored := press(t, m, " ")
	if ignored != nil {
		t.Fatal("skill modal allowed concurrent writes")
	}
	saved, ok := cmd().(savedMsg)
	if !ok || saved.err != nil {
		t.Fatalf("skill save failed: %#v", saved)
	}
	result, reload := m.Update(saved)
	m = result.(Model)
	if !m.skillModal.open || !m.loading || reload == nil {
		t.Fatal("skill modal did not refresh after saving")
	}
	updated := exampleConfig()
	updated.Skills[0].Harnesses = []string{"codex", "antigravity"}
	result, _ = m.Update(loadedMsg{config: updated})
	m = result.(Model)
	if !m.skillModal.open || !strings.Contains(m.View(), "[✓] Antigravity") {
		t.Fatalf("skill availability was not updated:\n%s", m.View())
	}
	m, _ = press(t, m, "esc")
	if m.skillModal.open {
		t.Fatal("Escape did not dismiss modal after editing")
	}
}

func TestSkillModalNoEnabledHarnessAndError(t *testing.T) {
	cfg := exampleConfig()
	cfg.Settings.Harnesses = nil
	m := modelFixture(cfg)
	result, _ := m.Update(loadedMsg{config: cfg})
	m = result.(Model)
	m, _ = press(t, m, "3")
	m, _ = press(t, m, "a")
	if m.skillModal.open || !strings.Contains(m.notice, "Enable") {
		t.Fatal("modal should not open without supported active harnesses")
	}
	cfg.Settings.Harnesses = []string{"codex"}
	m, _ = press(t, m, "a")
	m.service.editSkill = func(string, config.Skill, string, bool) error { return errors.New("cannot update YAML") }
	m, cmd := press(t, m, " ")
	result, _ = m.Update(cmd())
	m = result.(Model)
	if !m.skillModal.open || !strings.Contains(m.View(), "cannot update YAML") || m.busy {
		t.Fatalf("modal errors should display without closing dialog: %s", m.View())
	}
}

func TestHarnessToggleAndBusyState(t *testing.T) {
	cfg := exampleConfig()
	m := modelFixture(cfg)
	var toggled string
	m.service.toggle = func(_ string, name string, enable, expected bool) error {
		if name != "claude-code" || !enable || expected {
			t.Fatalf("incorrect toggle parameters %q, %v, %v", name, enable, expected)
		}
		toggled = name
		return nil
	}
	result, _ := m.Update(loadedMsg{config: cfg})
	m = result.(Model)
	m, cmd := press(t, m, " ")
	if !m.busy || cmd == nil {
		t.Fatal("harness toggle must start a save command")
	}
	// Navigation cannot initiate concurrent writes while the save is in flight.
	_, ignored := press(t, m, " ")
	if ignored != nil {
		t.Fatal("concurrent harness save was accepted")
	}
	saved, ok := cmd().(savedMsg)
	if !ok || saved.err != nil || toggled != "claude-code" {
		t.Fatalf("save did not use the expected operation: %#v", saved)
	}
	result, reload := m.Update(saved)
	m = result.(Model)
	if !m.loading || !strings.Contains(m.notice, "sync --apply") || reload == nil {
		t.Fatal("save did not trigger a source refresh")
	}
	result, _ = m.Update(reload())
	m = result.(Model)
	m, _ = press(t, m, "down")
	for i := 0; i < 5; i++ {
		m, _ = press(t, m, "down")
	}
	m, cmd = press(t, m, " ")
	if cmd != nil {
		t.Fatal("VS Code user-scope toggle must be disabled")
	}
}

func TestUntrustedDiagnosticsCannotInjectTerminalSequences(t *testing.T) {
	cfg := exampleConfig()
	cfg.Warnings = []string{"plugin: bad\x1b[2J\nnext line"}
	m := modelFixture(cfg)
	result, _ := m.Update(loadedMsg{config: cfg})
	m = result.(Model)
	m, _ = press(t, m, "5")
	if strings.Contains(m.View(), "\x1b[2J") || !strings.Contains(m.View(), "next line") {
		t.Fatal("warning contained unsanitized terminal controls or lost text")
	}
}

func TestMCPSharingChangeAndStatusError(t *testing.T) {
	cfg := exampleConfig()
	m := modelFixture(cfg)
	called := 0
	m.service.share = func(_ string, name, next, old string) error {
		called++
		if name != "one" || next != "isolated" || old != "shared" {
			t.Errorf("unexpected sharing request %q %q %q", name, next, old)
		}
		return errors.New("concurrent edit")
	}
	result, _ := m.Update(loadedMsg{config: cfg, status: broker.Status{Running: true}})
	m = result.(Model)
	m, _ = press(t, m, "2")
	m, cmd := press(t, m, "i")
	if cmd == nil {
		t.Fatal("i did not trigger sharing toggle")
	}
	result, _ = m.Update(cmd())
	m = result.(Model)
	if called != 1 || !strings.Contains(m.notice, "concurrent edit") {
		t.Fatal("sharing failure was suppressed")
	}
	result, _ = m.Update(statusMsg{err: errors.New("ipc unavailable")})
	if !strings.Contains(result.(Model).View(), "status error") {
		t.Fatal("broker diagnostics did not show status error")
	}
}

func TestLoadFailureAndHelp(t *testing.T) {
	m := modelFixture(exampleConfig())
	result, _ := m.Update(loadedMsg{err: errors.New("invalid agentmux.json")})
	m = result.(Model)
	if !strings.Contains(m.View(), "invalid agentmux.json") {
		t.Fatal("configuration error hidden")
	}
	m, _ = press(t, m, "?")
	if !strings.Contains(m.View(), "KEYBOARD REFERENCE") {
		t.Fatal("help overlay hidden")
	}
	m, _ = press(t, m, "?")
	m, cmd := press(t, m, "r")
	if !m.loading || cmd == nil {
		t.Fatal("refresh after a configuration failure unavailable")
	}
}
