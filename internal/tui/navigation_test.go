package tui

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"agentcfg/internal/config"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func assertAlignedStatusColumn(t *testing.T, options []huh.Option[string], count int) {
	t.Helper()
	if len(options) < count {
		t.Fatalf("only %d options, expected at least %d", len(options), count)
	}
	width := -1
	for i, option := range options[:count] {
		separator := strings.Index(option.Key, "  ·  ")
		if separator < 0 {
			t.Fatalf("missing status column in row %d: %q", i, option.Key)
		}
		actual := lipgloss.Width(option.Key[:separator])
		if width == -1 {
			width = actual
		} else if actual != width {
			t.Fatalf("status column offset differs: row %d width %d vs %d: %q", i, actual, width, option.Key)
		}
	}
}

func TestConfigurationAndInventoryStatusesAlignByVisibleWidth(t *testing.T) {
	cfg := &config.Config{
		Servers:  map[string]config.Server{"alpha": {}, "beta": {}},
		Skills:   []config.Skill{{Name: "review"}},
		Settings: config.Settings{Harnesses: []string{"codex", "cursor"}},
		Warnings: []string{"warning"},
	}
	configOptions := configNavigationOptions(cfg, 3)
	if len(configOptions) != 8 {
		t.Fatalf("expected eight CONFIG actions, got %d", len(configOptions))
	}
	assertAlignedStatusColumn(t, configOptions, 5)
	for i, expected := range []string{"enable / disable", "2 configured", "1 available", "3 folders", "1 warning(s)"} {
		if !strings.HasSuffix(configOptions[i].Key, expected) {
			t.Errorf("CONFIG option %d missing %q: %q", i, expected, configOptions[i].Key)
		}
	}
	if configOptions[7].Value != "exit" {
		t.Fatalf("Exit action moved unexpectedly: %+v", configOptions[7])
	}
	inventoryOptions := inventoryNavigationOptions(cfg, 3)
	if len(inventoryOptions) != 5 {
		t.Fatalf("expected five list actions, got %d", len(inventoryOptions))
	}
	assertAlignedStatusColumn(t, inventoryOptions, 4)
	for i, expected := range []string{"2 active", "2 configured", "1 available", "3 folders"} {
		if !strings.HasSuffix(inventoryOptions[i].Key, expected) {
			t.Errorf("LIST option %d missing %q: %q", i, expected, inventoryOptions[i].Key)
		}
	}
	wide := statusNavigationOptions([]navigationStatus{{title: "界", detail: "one", id: "a"}, {title: "e\u0301", detail: "two", id: "b"}, {title: "plain", detail: "three", id: "c"}})
	assertAlignedStatusColumn(t, wide, 3)
}

func TestNavigationColorsHaveReadableLightDarkAndDistinctRoles(t *testing.T) {
	for _, tc := range []struct {
		id, label string
		want      lipgloss.AdaptiveColor
	}{
		{"exit", "Exit", navExitColor},
		{"refresh", "Refresh", navFreshColor},
		{"diagnostics", "Status", navInfoColor},
		{"details", "Details", navInfoColor},
		{"back", "Back", navBackColor},
		{"sync", "Synchronize", navSyncColor},
		{"sharing", "Sharing", navActionColor},
		{"shared", "Shared", navActionColor},
		{"isolated", "Isolated", navWarnColor},
		{"skill", "Disabled skill", navWarnColor},
		{"mcp", "MCP servers", navActionColor},
	} {
		got := navigationColor(tc.id, tc.label)
		if got != tc.want || got.Light == "" || got.Dark == "" {
			t.Errorf("role %q/%q returned colors %+v, expected %+v", tc.id, tc.label, got, tc.want)
		}
	}
	if navExitColor == navActionColor || navBackColor == navFreshColor || navInfoColor == navActionColor {
		t.Fatal("Exit, Back, Refresh, status and actions must not be indistinguishable")
	}
}

func TestNavigationColoredSelectKeepsPlainSearchableKeysAndWidths(t *testing.T) {
	options := []huh.Option[string]{
		huh.NewOption("MCP servers  ·  0 configured", "mcp"),
		huh.NewOption("Refresh configuration", "refresh"),
		huh.NewOption("Exit", "exit"),
	}
	var selected string
	field := huh.NewSelect[string]().Title("Navigation").Options(options...).Value(&selected)
	renderer := lipgloss.NewRenderer(io.Discard)
	renderer.SetColorProfile(termenv.TrueColor)
	colored := &coloredNavigationSelect{Select: field, options: options, renderer: renderer}
	form := huh.NewForm(huh.NewGroup(colored)).WithTheme(Theme())
	_, _ = form.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
	plain := field.View()
	view := colored.View()
	if !strings.Contains(view, "\x1b[") {
		t.Fatalf("native navigation view lacks colors: %q", view)
	}
	for _, option := range options {
		if !strings.Contains(view, option.Key) {
			t.Errorf("colored view dropped option %q", option.Key)
		}
	}
	if lipgloss.Width(view) != lipgloss.Width(plain) {
		t.Fatalf("color codes altered visible layout width: colored=%d plain=%d", lipgloss.Width(view), lipgloss.Width(plain))
	}
	for _, option := range options {
		if strings.Contains(option.Key, "\x1b[") {
			t.Errorf("color codes polluted Huh Option.Key and would corrupt search: %q", option.Key)
		}
	}
	// Huh's native filter must match the label's actual zero, rather than a
	// digit in an ANSI foreground escape sequence on unrelated items.
	_, _ = colored.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	_, _ = colored.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("0")})
	filtered := colored.View()
	if !strings.Contains(filtered, "0 configured") || strings.Contains(filtered, "Refresh configuration") || strings.Contains(filtered, "Exit") {
		t.Fatalf("filtering by 0 included unrelated ANSI-colored choices: %q", filtered)
	}
}

func TestAccessibleNavigationDoesNotEmitANSICodesOrExtraBlankLines(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("AGENTCFG_ACCESSIBLE", "1")
	root := t.TempDir()
	var output bytes.Buffer
	if err := RunList(context.Background(), root, terminalByteReader{strings.NewReader("5\n")}, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "\x1b[") {
		t.Fatalf("accessible mode emitted terminal control sequences: %q", output.String())
	}
	if strings.HasSuffix(output.String(), "\n\n") {
		t.Fatalf("accessible form added trailing blank lines: %q", output.String())
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("accessible navigation wrote to temporary home: %v %v", entries, err)
	}
}

func TestColoredNavigationMatchesWholeOptionNotOverlappingLabel(t *testing.T) {
	options := []huh.Option[string]{
		huh.NewOption("Exit", "exit"),
		huh.NewOption("Exit to parent menu", "back"),
		huh.NewOption("Refresh", "refresh"),
	}
	var selected string
	field := huh.NewSelect[string]().Title("Navigation").Options(options...).Value(&selected)
	renderer := lipgloss.NewRenderer(io.Discard)
	renderer.SetColorProfile(termenv.TrueColor)
	colored := &coloredNavigationSelect{Select: field, options: options, renderer: renderer}
	form, group := newNaturalForm(strings.NewReader(""), io.Discard, colored)
	resizeNaturalForm(form, group, []huh.Field{colored}, 80)
	_, _ = form.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	view := colored.View()
	red := renderer.NewStyle().Foreground(navExitColor).Render("Exit")
	amber := renderer.NewStyle().Foreground(navBackColor).Render("Exit to parent menu")
	cyan := renderer.NewStyle().Foreground(navFreshColor).Render("Refresh")
	for _, item := range []string{red, amber, cyan} {
		if count := strings.Count(view, item); count != 1 {
			t.Fatalf("expected one correct option color (got %d) for %q in view:\n%s", count, item, view)
		}
	}
	if lipgloss.Width(view) != lipgloss.Width(field.View()) {
		t.Fatal("colorizing ambiguous labels altered visible width")
	}
}

func TestNavigationPlainModeRespectsNoColorAndExplicitAccessibility(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("CLICOLOR", "1")
	t.Setenv("AGENTCFG_ACCESSIBLE", "")
	if !navigationPlainMode() {
		t.Fatal("NO_COLOR must prohibit UI option coloring")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR", "0")
	if !navigationPlainMode() {
		t.Fatal("CLICOLOR=0 must prohibit UI option coloring")
	}
	t.Setenv("CLICOLOR", "1")
	t.Setenv("AGENTCFG_ACCESSIBLE", "1")
	if !navigationPlainMode() {
		t.Fatal("accessible Huh prompts must be plain")
	}
	// A non-terminal reader/writer never gains ANSI even with color requested.
	s := interfaceSession{ctx: context.Background(), in: strings.NewReader("1\n"), out: &bytes.Buffer{}}
	if s.navigationSupportsColor() {
		t.Fatal("non-TTY screen unexpectedly enabled option coloring")
	}
}
