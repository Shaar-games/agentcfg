package tui

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"agentcfg/internal/config"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

var (
	navActionColor = lipgloss.AdaptiveColor{Light: "#17664E", Dark: "#83D9AF"}
	navInfoColor   = lipgloss.AdaptiveColor{Light: "#315A79", Dark: "#A9CDE6"}
	navExitColor   = lipgloss.AdaptiveColor{Light: "#AF272E", Dark: "#FF828A"}
	navBackColor   = lipgloss.AdaptiveColor{Light: "#895D14", Dark: "#FFD182"}
	navFreshColor  = lipgloss.AdaptiveColor{Light: "#096883", Dark: "#7CDFFF"}
	navSyncColor   = lipgloss.AdaptiveColor{Light: "#64469B", Dark: "#D0B3FF"}
	navWarnColor   = lipgloss.AdaptiveColor{Light: "#925B17", Dark: "#F3C274"}
)

// The CONFIG status column is padded by terminal cell width, not bytes or
// runes. This also works if a label later contains wide or combining glyphs.
type navigationStatus struct {
	title, detail, id string
}

// Item values are namespaced away from action sentinels (back/exit/refresh).
// A legitimate MCP or skill called "back" must remain selectable; this prefix
// is solely an internal Huh Option.Value and never changes its displayed name.
const navigationItemPrefix = "item:"

func navigationItemValue(id string) string { return navigationItemPrefix + id }

func navigationItemName(value string) (string, bool) {
	return strings.CutPrefix(value, navigationItemPrefix)
}

func statusNavigationOptions(rows []navigationStatus) []huh.Option[string] {
	width := 0
	for _, row := range rows {
		width = max(width, lipgloss.Width(row.title))
	}
	options := make([]huh.Option[string], 0, len(rows))
	for _, row := range rows {
		padding := strings.Repeat(" ", width-lipgloss.Width(row.title))
		options = append(options, huh.NewOption(row.title+padding+"  ·  "+row.detail, row.id))
	}
	return options
}

func configNavigationOptions(cfg *config.Config, plugins int) []huh.Option[string] {
	options := statusNavigationOptions([]navigationStatus{
		{title: "Harnesses", detail: "enable / disable", id: "harnesses"},
		{title: "MCP servers", detail: fmt.Sprintf("%d configured", len(cfg.Servers)), id: "mcp"},
		{title: "Skills", detail: fmt.Sprintf("%d available", len(cfg.Skills)), id: "skills"},
		{title: "Agent Plugins", detail: fmt.Sprintf("%d folders", plugins), id: "plugins"},
		{title: "Diagnostics and broker status", detail: fmt.Sprintf("%d warning(s)", len(cfg.Warnings)), id: "diagnostics"},
	})
	return append(options,
		huh.NewOption("Synchronize native harnesses", "sync"),
		huh.NewOption("Refresh configuration and status", "refresh"),
		huh.NewOption("Exit", "exit"))
}

func inventoryNavigationOptions(cfg *config.Config, plugins int) []huh.Option[string] {
	options := statusNavigationOptions([]navigationStatus{
		{title: "Harnesses", detail: fmt.Sprintf("%d active", len(cfg.Settings.Harnesses)), id: "harnesses"},
		{title: "MCP servers", detail: fmt.Sprintf("%d configured", len(cfg.Servers)), id: "mcp"},
		{title: "Skills", detail: fmt.Sprintf("%d available", len(cfg.Skills)), id: "skills"},
		{title: "Agent Plugins", detail: fmt.Sprintf("%d folders", plugins), id: "plugins"},
	})
	return append(options, huh.NewOption("Exit", "exit"))
}

// A coloured Select must retain plain Option.Key values. Huh's built-in
// filtering compares strings directly, so ANSI in options would make e.g. a
// search for "0" match unrelated choices via the SGR escape codes.
func navigationColor(id, label string) lipgloss.AdaptiveColor {
	switch id {
	case "exit":
		return navExitColor
	case "back":
		return navBackColor
	case "refresh":
		return navFreshColor
	case "sync":
		return navSyncColor
	case "diagnostics", "details":
		return navInfoColor
	case "isolated":
		return navWarnColor
	}
	switch {
	case strings.Contains(strings.ToLower(label), "unavailable"),
		strings.Contains(strings.ToLower(label), "warning"),
		strings.Contains(strings.ToLower(label), "disabled"):
		return navWarnColor
	default:
		return navActionColor
	}
}

func (s interfaceSession) navigationSupportsColor() bool {
	// Accessible prompts display plain numbered options. Preserve their labels
	// for screen readers, piping, TERM=dumb and non-interactive tests.
	if navigationPlainMode() {
		return false
	}
	in, inOK := s.in.(*os.File)
	out, outOK := s.out.(*os.File)
	return inOK && outOK && term.IsTerminal(in.Fd()) && term.IsTerminal(out.Fd())
}

func navigationPlainMode() bool {
	// Respect the established NO_COLOR convention even on color-capable TTYs.
	// Huh itself may still run native forms when a Windows console reports a
	// dumb TERM, so keep that platform exception separate from accessibility.
	if os.Getenv("AGENTCFG_ACCESSIBLE") == "1" || os.Getenv("NO_COLOR") != "" || os.Getenv("CLICOLOR") == "0" {
		return true
	}
	return runtime.GOOS != "windows" && strings.EqualFold(os.Getenv("TERM"), "dumb")
}

func (s interfaceSession) navigationSelect(field *huh.Select[string], options []huh.Option[string]) huh.Field {
	if !s.navigationSupportsColor() {
		return field
	}
	return &coloredNavigationSelect{Select: field, options: options, renderer: lipgloss.NewRenderer(s.out)}
}

// The wrapper only adds ANSI to the final rendered view, after Huh has
// filtered and laid out its untouched labels. It retains the official Select
// focus/navigation/size behaviour and delegates accessible prompts to Huh.
type coloredNavigationSelect struct {
	*huh.Select[string]
	options  []huh.Option[string]
	renderer *lipgloss.Renderer
}

func (s *coloredNavigationSelect) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := s.Select.Update(msg)
	return s, cmd
}

func (s *coloredNavigationSelect) View() string {
	lines := strings.Split(s.Select.View(), "\n")
	for i, line := range lines {
		plain := strings.TrimRight(ansi.Strip(line), " ")
		// Check the longest suffix first, so a `Back` option never colors the
		// `Back` prefix inside a distinct `Back to menu` row. The option label
		// remains unmodified for Huh's search/filter logic.
		best := -1
		for j, option := range s.options {
			if option.Key != "" && strings.HasSuffix(plain, option.Key) && (best < 0 || len(option.Key) > len(s.options[best].Key)) {
				best = j
			}
		}
		if best < 0 {
			continue
		}
		option := s.options[best]
		start := strings.LastIndex(line, option.Key)
		if start < 0 {
			continue
		}
		styled := s.renderer.NewStyle().Foreground(navigationColor(option.Value, option.Key)).Render(option.Key)
		lines[i] = line[:start] + styled + line[start+len(option.Key):]
	}
	return strings.Join(lines, "\n")
}
