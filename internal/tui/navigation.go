package tui

import (
	"os"
	"runtime"
	"strconv"
	"strings"

	"agentcfg/internal/config"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// Status columns are padded by terminal cell width, not bytes or
// runes. This also works if a label later contains wide or combining glyphs.
type navigationStatus struct {
	title, detail, id string
	columns           []string
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
	// Measure every column in terminal cells. Keep keys ANSI-free for Huh's
	// filtering; wide glyphs and combining accents must not shift columns.
	cells := make([][]string, len(rows))
	var widths []int
	for i, row := range rows {
		cells[i] = append([]string{row.title}, row.columns...)
		if len(row.columns) == 0 {
			cells[i] = append(cells[i], row.detail)
		}
		for j, cell := range cells[i] {
			cell = singleLineLabel(cell)
			cells[i][j] = cell
			if j == len(widths) {
				widths = append(widths, 0)
			}
			widths[j] = max(widths[j], lipgloss.Width(cell))
		}
	}
	options := make([]huh.Option[string], 0, len(rows))
	for i, row := range rows {
		last := len(cells[i]) - 1
		for last > 0 && cells[i][last] == "" {
			last--
		}
		var label strings.Builder
		for j, cell := range cells[i][:last+1] {
			label.WriteString(cell)
			if j < last {
				label.WriteString(strings.Repeat(" ", widths[j]-lipgloss.Width(cell)))
				label.WriteString("  ·  ")
			}
		}
		options = append(options, huh.NewOption(label.String(), row.id))
	}
	return options
}

// Configuration and the read-only inventory share the same compact overview.
// Counts reflect the loaded configuration, not the number of supported adapters.
func overviewNavigationOptions(cfg *config.Config, plugins int) []huh.Option[string] {
	options := statusNavigationOptions([]navigationStatus{
		{title: "Harnesses", detail: strconv.Itoa(len(cfg.Settings.Harnesses)), id: "harnesses"},
		{title: "MCP servers", detail: strconv.Itoa(len(cfg.Servers)), id: "mcp"},
		{title: "Skills", detail: strconv.Itoa(len(cfg.Skills)), id: "skills"},
		{title: "Agent Plugins", detail: strconv.Itoa(plugins), id: "plugins"},
	})
	return append(options,
		huh.NewOption("Refresh", "refresh"),
		huh.NewOption("Exit", "exit"))
}

// A coloured Select must retain plain Option.Key values. Huh's built-in
// filtering compares strings directly, so ANSI in options would make e.g. a
// search for "0" match unrelated choices via the SGR escape codes.
func navigationColor(id, _ string) lipgloss.AdaptiveColor {
	switch id {
	case "exit":
		return navExitColor
	case "back":
		return navBackColor
	case "refresh":
		return navFreshColor
	case "details":
		return navInfoColor
	case "isolated":
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
	view := &navigationField{Select: field, options: options}
	if s.navigationSupportsColor() {
		view.renderer = lipgloss.NewRenderer(s.out)
	}
	return view
}

// Navigation decorates Huh's native view with optional colors and a bottom
// Back row. Options stay untouched: spacing never becomes a selectable item
// or part of a search label, and accessible prompts retain native numbering.
type navigationField struct {
	*huh.Select[string]
	options    []huh.Option[string]
	renderer   *lipgloss.Renderer
	optionRows int
}

func (s *navigationField) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := s.Select.Update(msg)
	return s, cmd
}

func (s *navigationField) View() string {
	view := padNavigationBack(s.Select, s.options, s.optionRows)
	if s.renderer == nil {
		return view
	}
	lines := strings.Split(view, "\n")
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
		styled := s.renderOptionLabel(option)
		lines[i] = line[:start] + styled + line[start+len(option.Key):]
	}
	return strings.Join(lines, "\n")
}

func singleLineLabel(value string) string {
	return strings.Join(strings.Fields(ansi.Strip(value)), " ")
}

// Apply semantic colors after Huh filtering/layout. Each status cell has its
// own role; a transport or sharing status must not color the whole row green.
func (s *navigationField) renderOptionLabel(option huh.Option[string]) string {
	cells := strings.Split(option.Key, "  \u00b7  ")
	for i, cell := range cells {
		color := navigationColor(option.Value, cell)
		if i > 0 {
			switch strings.TrimSpace(strings.ToLower(cell)) {
			case "shared", "enabled":
				color = navActionColor
			case "isolated", "warning", "disabled", "adapter unavailable":
				color = navWarnColor
			default:
				color = navInfoColor
			}
		}
		cells[i] = s.renderer.NewStyle().Foreground(color).Render(cell)
	}
	return strings.Join(cells, "  \u00b7  ")
}
