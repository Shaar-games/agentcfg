package tui

import (
	"bytes"
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"agentcfg/internal/config"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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

func TestOverviewCountsAreCompactAlignedAndReflectConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		plugins int
		counts  []string
	}{
		{name: "empty", counts: []string{"0", "0", "0", "0"}},
		{name: "one", cfg: config.Config{
			Settings: config.Settings{Harnesses: []string{"cursor"}},
			Servers:  map[string]config.Server{"alpha": {}},
			Skills:   []config.Skill{{Name: "review"}},
		}, plugins: 1, counts: []string{"1", "1", "1", "1"}},
		{name: "multiple", cfg: config.Config{
			Settings: config.Settings{Harnesses: []string{"claude-code", "codex", "cursor"}},
			Servers:  map[string]config.Server{"alpha": {}, "beta": {}},
			Skills:   make([]config.Skill, 12),
			Warnings: []string{"warning must not add a diagnostic menu entry"},
		}, plugins: 10, counts: []string{"3", "2", "12", "10"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := overviewNavigationOptions(&tc.cfg, tc.plugins)
			if len(options) != 6 {
				t.Fatalf("expected exactly six overview actions, got %d", len(options))
			}
			assertAlignedStatusColumn(t, options, 4)
			for i, label := range []string{"Harnesses", "MCP servers", "Skills", "Agent Plugins"} {
				title, count, found := strings.Cut(options[i].Key, "  ·  ")
				if !found || strings.TrimSpace(title) != label || count != tc.counts[i] {
					t.Errorf("row %d = %q; want %q with only counter %s", i, options[i].Key, label, tc.counts[i])
				}
			}
			var ids []string
			for _, option := range options {
				ids = append(ids, option.Value)
			}
			if !slices.Equal(ids, []string{"harnesses", "mcp", "skills", "plugins", "refresh", "exit"}) {
				t.Fatalf("overview order or actions changed: %v", ids)
			}
			if options[4].Key != "Refresh" || options[5].Key != "Exit" {
				t.Fatalf("actions must use short labels: %+v", options[4:])
			}
		})
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
		{"details", "Details", navInfoColor},
		{"back", "Back", navBackColor},
		{"sharing", "Sharing", navActionColor},
		{"shared", "Shared", navActionColor},
		{"isolated", "Isolated", navWarnColor},
		{"skill", "Disabled skill", navActionColor},
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
		huh.NewOption("MCP servers  ·  0", "mcp"),
		huh.NewOption("Refresh", "refresh"),
		huh.NewOption("Exit", "exit"),
	}
	var selected string
	field := huh.NewSelect[string]().Title("Navigation").Options(options...).Value(&selected)
	renderer := lipgloss.NewRenderer(io.Discard)
	renderer.SetColorProfile(termenv.TrueColor)
	colored := &navigationField{Select: field, options: options, renderer: renderer}
	form := huh.NewForm(huh.NewGroup(colored)).WithTheme(Theme())
	_, _ = form.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
	plain := field.View()
	view := colored.View()
	if !strings.Contains(view, "\x1b[") {
		t.Fatalf("native navigation view lacks colors: %q", view)
	}
	for _, option := range options {
		if !strings.Contains(ansi.Strip(view), option.Key) {
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
	if !strings.Contains(ansi.Strip(filtered), "MCP servers  ·  0") || strings.Contains(filtered, "Refresh") || strings.Contains(filtered, "Exit") {
		t.Fatalf("filtering by 0 included unrelated ANSI-colored choices: %q", filtered)
	}
}

func TestAccessibleNavigationDoesNotEmitANSICodesOrExtraBlankLines(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("AGENTCFG_ACCESSIBLE", "1")
	root := t.TempDir()
	var output bytes.Buffer
	if err := RunList(context.Background(), root, strings.NewReader("6\n"), &output); err != nil {
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
	colored := &navigationField{Select: field, options: options, renderer: renderer}
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

func TestAllStatusColumnsAlignWithUnicodeAndOptionalCells(t *testing.T) {
	rows := []navigationStatus{
		{title: "界", columns: []string{"stdio", "shared", ""}, id: "first"},
		{title: "e\u0301", columns: []string{"streamable-http", "isolated", "warning"}, id: "second"},
		{title: "a long name", columns: []string{"sse", "shared", "warning"}, id: "third"},
	}
	options := statusNavigationOptions(rows)
	offsets := map[int]int{}
	for _, option := range options {
		cells := strings.Split(option.Key, "  \u00b7  ")
		prefix := ""
		for j, cell := range cells {
			if j > 0 {
				actual := lipgloss.Width(prefix)
				if want, ok := offsets[j]; ok && want != actual {
					t.Fatalf("column %d moved: %d != %d in %q", j, actual, want, option.Key)
				}
				offsets[j] = actual
			}
			prefix += cell + "  \u00b7  "
		}
		if strings.ContainsAny(option.Key, "\r\n\t\x1b") {
			t.Fatalf("unsafe column label: %q", option.Key)
		}
	}
}

func TestStatusColorsPreserveCellsAndDistinguishSharing(t *testing.T) {
	renderer := lipgloss.NewRenderer(io.Discard)
	renderer.SetColorProfile(termenv.TrueColor)
	wrapper := &navigationField{renderer: renderer}
	for _, status := range []string{"shared", "isolated", "warning", "disabled"} {
		key := "warning-guide  \u00b7  stdio  \u00b7  " + status
		rendered := wrapper.renderOptionLabel(huh.NewOption(key, "item:warning-guide"))
		color := navWarnColor
		if status == "shared" {
			color = navActionColor
		}
		if !strings.Contains(rendered, renderer.NewStyle().Foreground(color).Render(status)) {
			t.Fatalf("wrong status color: %q", rendered)
		}
		if ansi.Strip(rendered) != key || lipgloss.Width(rendered) != lipgloss.Width(key) {
			t.Fatal("color changed column layout")
		}
	}
}
