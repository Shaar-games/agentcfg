package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const visibleMenuOptions = 10

// Pin the action outside the scrollable body. Both navigation and checkbox
// menus use native Huh rows and the same terminal-cell cursor handling.
func hasMenuCursor(line string) bool {
	frame := Theme().Focused.Base.GetHorizontalFrameSize()
	return ansi.Strip(ansi.Cut(line, frame, frame+2)) == "> "
}

func emptyMenuLine(line string) bool {
	frame := Theme().Focused.Base.GetHorizontalFrameSize()
	return strings.TrimSpace(ansi.Strip(ansi.Cut(line, frame, lipgloss.Width(line)))) == ""
}

func menuOptionLabel(line string) string {
	frame := Theme().Focused.Base.GetHorizontalFrameSize()
	return strings.TrimSpace(ansi.Strip(ansi.Cut(line, frame+2, lipgloss.Width(line))))
}

// Measure a wrapped native option without reproducing Huh's word wrapping.
// Whitespace includes layout padding and separators; all label bytes remain.
func menuOptionEnd(lines []string, start int, label string) int {
	remaining := strings.Join(strings.Fields(label), "")
	for i := start; i < len(lines); i++ {
		part := strings.Join(strings.Fields(menuOptionLabel(lines[i])), "")
		var ok bool
		remaining, ok = strings.CutPrefix(remaining, part)
		if !ok {
			break
		}
		if remaining == "" {
			return i + 1
		}
	}
	return start + 1
}

func renderPinnedAction(view string, header, rows int, label string, active bool) string {
	if active {
		lines := strings.Split(view, "\n")
		frame := Theme().Focused.Base.GetHorizontalFrameSize()
		for i := header; i < len(lines); i++ {
			if hasMenuCursor(lines[i]) {
				lines[i] = ansi.Cut(lines[i], 0, frame) + "  " + ansi.Cut(lines[i], frame+2, lipgloss.Width(lines[i]))
			}
		}
		view = strings.Join(lines, "\n")
	}
	cursor := "  "
	if active {
		cursor = "> "
	}
	base := Theme().Focused.Base
	line := base.Width(max(0, lipgloss.Width(view)-base.GetHorizontalBorderSize())).Render(cursor + label)
	return padBottomAction(view+"\n"+line, header, rows)
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
