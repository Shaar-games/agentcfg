package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const visibleMenuOptions = 10

// Reserve the last option row for Back on short navigation menus. Decorate the
// rendered view rather than adding dummy options: one Down still moves directly
// from the last item to Back, and accessible numbering never contains spacers.
func padNavigationBack(field *huh.Select[string], options []huh.Option[string], rows int) string {
	view := field.View()
	if len(options) == 0 || len(options) > visibleMenuOptions || !isTerminalAction(options[len(options)-1].Value) || field.GetFiltering() {
		return view
	}
	lines := strings.Split(view, "\n")
	back := len(lines) - 1
	if !strings.HasSuffix(strings.TrimSpace(ansi.Strip(lines[back])), options[len(options)-1].Key) {
		return view
	}
	// Measure Huh's own wrapped header independently of its option viewport.
	probe := *field
	probe.Height(1)
	header := lipgloss.Height(probe.View()) - 1
	return padBottomAction(view, header, rows)
}

func isTerminalAction(id string) bool { return id == "back" || id == "exit" }

// All bottom actions use the same row budget and visual-only spacing.
func padBottomAction(view string, header, rows int) string {
	lines := strings.Split(view, "\n")
	back := len(lines) - 1
	if rows <= 0 {
		rows = visibleMenuOptions
	}
	gap := rows - (len(lines) - header)
	if gap <= 0 {
		return view
	}
	// Preserve the native left border/padding without copying its cursor.
	frame := Theme().Focused.Base.GetHorizontalFrameSize()
	blank := ansi.Truncate(lines[back], frame, "") + strings.Repeat(" ", max(0, lipgloss.Width(lines[back])-frame))
	padding := strings.Repeat(blank+"\n", gap)
	prefix := strings.Join(lines[:back], "\n")
	if back > 0 {
		prefix += "\n"
	}
	return prefix + padding + lines[back]
}

// Huh Height includes the title and wrapped description, not just the options.
// Measure its native one-row view to reserve that header space without copying
// Huh's wrapping logic. Short lists keep their natural height. All string
// selectors, including the color wrapper and wizard multiselects, share this.
func probeMenuField(field huh.Field) huh.Field {

	switch f := field.(type) {
	case *huh.Select[string]:
		copy := *f
		return &copy
	case *navigationField:
		copy := *f.Select
		return &copy
	case *backMultiSelect:
		copy := *f.MultiSelect
		return &copy
	case *huh.MultiSelect[string]:
		copy := *f
		return &copy
	default:
		return nil
	}
}

func menuHeaderHeight(field huh.Field) int {
	probe := probeMenuField(field)
	if probe == nil {
		return 0
	}
	probe.WithHeight(1)
	return lipgloss.Height(probe.View()) - 1
}

func sizeMenuField(field huh.Field, rows int) {
	if note, ok := field.(*scrollableNote); ok {
		note.WithHeight(rows)
		return
	}
	probe := probeMenuField(field)
	if probe == nil {
		return
	}

	probe.WithHeight(0)
	_, _ = probe.Update(nil)
	natural := lipgloss.Height(probe.View())
	probe.WithHeight(1)
	headerAndRow := lipgloss.Height(probe.View())
	limit := headerAndRow + rows - 1
	if natural <= limit {
		// A positive Huh Select height resets its viewport offset to the
		// cursor on every update, even when every option fits. Natural height
		// avoids that surprising scrolling on short lists.
		field.WithHeight(0)
		var single *huh.Select[string]
		switch f := field.(type) {
		case *huh.Select[string]:
			single = f
		case *navigationField:
			single = f.Select
		}
		if single != nil && !single.GetFiltering() {
			// Native navigation clamps the old offset after a list grows back
			// to its full height. Restore the cursor without changing its value.
			_, _ = single.Update(tea.KeyMsg{Type: tea.KeyUp})
			_, _ = single.Update(tea.KeyMsg{Type: tea.KeyDown})
		}
	} else {
		field.WithHeight(limit)
	}
	_, _ = field.Update(nil)
}

// Reserve the wrapped header, Huh footer and renderer's final newline before
// filling the remaining option area. Blank space must never trigger overflow.
func sizeNavigationRows(nav bottomActionLayout, group *huh.Group, terminalHeight int) {
	nav.setOptionRows(visibleMenuOptions)
	if terminalHeight <= 0 {
		return
	}
	header := nav.headerHeight()
	chrome := header + 2
	if text := group.Header(); text != "" {
		chrome += lipgloss.Height(text)
	}
	if text := group.Footer(); text != "" {
		chrome += lipgloss.Height(text) + 1
	}
	nav.setOptionRows(max(1, min(visibleMenuOptions, terminalHeight-chrome)))
}

// Both single-choice actions and checkbox menus reserve the same bottom area.
type bottomActionLayout interface {
	headerHeight() int
	setOptionRows(int)
}

func (s *navigationField) headerHeight() int {
	probe := *s.Select
	probe.Height(1)
	return lipgloss.Height(probe.View()) - 1
}
func (s *navigationField) setOptionRows(rows int) { s.optionRows = rows }
