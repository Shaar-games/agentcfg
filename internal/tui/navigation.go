package tui

import (
	"os"
	"runtime"
	"strconv"
	"strings"

	"agentcfg/internal/config"

	"github.com/charmbracelet/bubbles/key"
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
	bodyOffset int
}

func (s *navigationField) hasBottomAction() bool {
	return len(s.options) > 0 && isTerminalAction(s.options[len(s.options)-1].Value)
}

func (s *navigationField) normalizeViewport() {
	// Let Huh render every option, then window that native view ourselves. Huh
	// v1 resets a positive-height Select's offset to its cursor on each update.
	// Natural height plus native movement clamps its viewport back to zero.
	s.Select.Height(0)
	if _, ok := s.Hovered(); ok {
		_, _ = s.Select.Update(tea.KeyMsg{Type: tea.KeyUp})
		_, _ = s.Select.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
}

func (s *navigationField) WithHeight(height int) huh.Field {
	if s.hasBottomAction() {
		s.normalizeViewport()
	} else {
		s.Select.WithHeight(height)
	}
	return s
}

func (s *navigationField) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if s.hasBottomAction() {
		if input, ok := msg.(tea.KeyMsg); ok && (input.String() == "tab" || input.String() == "esc" && !s.GetFiltering()) {
			// Leave and clear a filter before selecting the real terminal action.
			// It remains an ordinary Huh option, including in accessible prompts.
			_, _ = s.Select.Update(tea.KeyMsg{Type: tea.KeyEsc})
			_, _ = s.Select.Update(tea.KeyMsg{Type: tea.KeyEsc})
			_, cmd := s.Select.Update(tea.KeyMsg{Type: tea.KeyEnd})
			s.normalizeViewport()
			return s, cmd
		}
	}
	_, cmd := s.Select.Update(msg)
	if s.hasBottomAction() {
		s.normalizeViewport()
	}
	return s, cmd
}

func (s *navigationField) KeyBinds() []key.Binding {
	bindings := s.Select.KeyBinds()
	if s.hasBottomAction() {
		action := s.options[len(s.options)-1].Value
		bindings = append(bindings, key.NewBinding(key.WithKeys("esc", "tab"), key.WithHelp("esc/tab", action)))
	}
	return bindings
}

func (s *navigationField) pinnedView() string {
	view := s.Select.View()
	if !s.hasBottomAction() {
		return view
	}
	header := s.headerHeight()
	lines := strings.Split(view, "\n")
	header = min(header, len(lines))
	body := lines[header:]
	// Huh pads filtered results to the unfiltered option count. Remove only
	// empty native rows, then detach the real Back/Exit row if it is visible.
	for len(body) > 0 && emptyMenuLine(body[len(body)-1]) {
		body = body[:len(body)-1]
	}
	action := s.options[len(s.options)-1]
	if len(body) > 0 && menuOptionLabel(body[len(body)-1]) == action.Key {
		body = body[:len(body)-1]
	}
	value, ok := s.Hovered()
	active := ok && value == action.Value && !s.GetFiltering()
	rows := s.optionRows
	if rows <= 0 {
		rows = visibleMenuOptions
	}
	limit := max(1, rows-1)
	if !active {
		for i, line := range body {
			if hasMenuCursor(line) {
				end := i + 1
				for _, option := range s.options {
					if ok && option.Value == value {
						end = menuOptionEnd(body, i, option.Key)
						break
					}
				}
				if end > s.bodyOffset+limit {
					s.bodyOffset = end - limit
				}
				if i < s.bodyOffset {
					s.bodyOffset = i
				}
				break
			}
		}
	}
	s.bodyOffset = max(0, min(s.bodyOffset, len(body)-limit))
	end := min(len(body), s.bodyOffset+limit)
	window := append([]string(nil), lines[:header]...)
	window = append(window, body[s.bodyOffset:end]...)
	return renderPinnedAction(strings.Join(window, "\n"), header, rows, action.Key, active)
}

func (s *navigationField) View() string {
	view := s.pinnedView()
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
