package tui

import (
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

var (
	navActionColor = lipgloss.AdaptiveColor{Light: "#17664E", Dark: "#83D9AF"}
	navInfoColor   = lipgloss.AdaptiveColor{Light: "#315A79", Dark: "#A9CDE6"}
	navExitColor   = lipgloss.AdaptiveColor{Light: "#AF272E", Dark: "#FF828A"}
	navBackColor   = lipgloss.AdaptiveColor{Light: "#895D14", Dark: "#FFD182"}
	navFreshColor  = lipgloss.AdaptiveColor{Light: "#096883", Dark: "#7CDFFF"}
	navWarnColor   = lipgloss.AdaptiveColor{Light: "#925B17", Dark: "#F3C274"}
)

// Theme keeps Huh's native forms, filtering and checkbox behaviour, but uses
// restrained neutral text instead of the default violet bold headings.
func Theme() *huh.Theme {
	t := huh.ThemeBase()
	ink := lipgloss.AdaptiveColor{Light: "#263445", Dark: "#D9E1EA"}
	muted := lipgloss.AdaptiveColor{Light: "#667588", Dark: "#9AA8B8"}
	line := lipgloss.AdaptiveColor{Light: "#BCC7D3", Dark: "#596779"}
	accent := navInfoColor
	button := lipgloss.AdaptiveColor{Light: "#E5ECF1", Dark: "#354454"}

	t.Focused.Base = t.Focused.Base.BorderForeground(line)
	t.Focused.Card = t.Focused.Base
	t.Focused.Title = lipgloss.NewStyle().Foreground(ink).Bold(false)
	t.Focused.NoteTitle = t.Focused.Title
	t.Focused.Description = lipgloss.NewStyle().Foreground(muted)
	t.Focused.Option = lipgloss.NewStyle().Foreground(ink)
	t.Focused.SelectSelector = t.Focused.SelectSelector.Foreground(accent)
	t.Focused.MultiSelectSelector = t.Focused.MultiSelectSelector.Foreground(accent)
	t.Focused.SelectedOption = lipgloss.NewStyle().Foreground(ink).Bold(false)
	t.Focused.UnselectedOption = lipgloss.NewStyle().Foreground(ink)
	t.Focused.SelectedPrefix = lipgloss.NewStyle().Foreground(accent).SetString("[x] ")
	t.Focused.UnselectedPrefix = lipgloss.NewStyle().Foreground(muted).SetString("[ ] ")
	t.Focused.NextIndicator = t.Focused.NextIndicator.Foreground(accent)
	t.Focused.PrevIndicator = t.Focused.PrevIndicator.Foreground(accent)
	t.Focused.FocusedButton = t.Focused.FocusedButton.Foreground(ink).Background(button).Bold(false)
	t.Focused.BlurredButton = t.Focused.BlurredButton.Foreground(muted).Bold(false)
	t.Focused.TextInput.Prompt = t.Focused.TextInput.Prompt.Foreground(accent)
	t.Focused.TextInput.Placeholder = t.Focused.TextInput.Placeholder.Foreground(muted)
	t.Focused.TextInput.Cursor = t.Focused.TextInput.Cursor.Foreground(accent)
	t.Focused.ErrorIndicator = t.Focused.ErrorIndicator.Foreground(navExitColor)
	t.Focused.ErrorMessage = t.Focused.ErrorMessage.Foreground(navExitColor)

	t.Blurred = t.Focused
	t.Blurred.Base = t.Blurred.Base.BorderStyle(lipgloss.HiddenBorder())
	t.Blurred.Card = t.Blurred.Base
	t.Blurred.MultiSelectSelector = lipgloss.NewStyle().SetString("  ")
	t.Blurred.SelectSelector = lipgloss.NewStyle().SetString("  ")
	t.Group.Title = t.Focused.Title
	t.Group.Description = t.Focused.Description
	return t
}
