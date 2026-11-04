package tui

import (
	"fmt"
	"io"
	"slices"

	"agentcfg/internal/config"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

// Confirmation keeps one action title, optional review data and a negative
// default across CLI commands and configuration menus.
func Confirmation(title, description string, confirmed *bool) huh.Field {
	return &confirmationField{
		Confirm: huh.NewConfirm().Title(title).Description(description).
			Affirmative("Yes!").Negative("No.").Value(confirmed),
		description: description,
	}
}

type confirmationField struct {
	*huh.Confirm
	description string
}

func (f *confirmationField) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := f.Confirm.Update(msg)
	return f, cmd
}

func (f *confirmationField) WithAccessible(accessible bool) huh.Field {
	f.Confirm.WithAccessible(accessible)
	return f
}

// Huh omits confirmation descriptions in accessible mode. Preserve review data
// once, including when a small native terminal falls back to accessible output.
func (f *confirmationField) RunAccessible(w io.Writer, r io.Reader) error {
	if f.description != "" {
		if _, err := fmt.Fprintln(w, f.description); err != nil {
			return err
		}
	}
	return f.Confirm.RunAccessible(w, r)
}

// HarnessOptions uses the same labels and ordering everywhere. A nil available
// set means all supported harnesses; an empty set means none.
func HarnessOptions(available, checked []string) []huh.Option[string] {
	var options []huh.Option[string]
	for _, h := range harnesses {
		if h.UserScopeEnabled && (available == nil || slices.Contains(available, h.ID)) {
			options = append(options, huh.NewOption(h.Label, h.ID).Selected(slices.Contains(checked, h.ID)))
		}
	}
	return options
}

// HarnessNames renders user-facing names instead of internal identifiers.
func HarnessNames(ids []string) []string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		label := id
		for _, h := range harnesses {
			if h.ID == id {
				label = h.Label
				break
			}
		}
		names = append(names, label)
	}
	return names
}

func harnessAvailability(available, filter []string) []string {
	var checked []string
	for _, id := range available {
		if config.Allows(filter, id) {
			checked = append(checked, id)
		}
	}
	return checked
}
