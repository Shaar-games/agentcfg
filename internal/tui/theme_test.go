package tui

import (
	"strings"
	"testing"
)

func TestThemeUsesNeutralUnboldedHeadingsAndNativeCheckboxPrefixes(t *testing.T) {
	theme := Theme()
	if theme.Focused.Title.GetBold() || theme.Focused.SelectedOption.GetBold() {
		t.Fatal("new Huh theme should not use bold violet headings/options")
	}
	if strings.Contains(strings.ToLower(theme.Focused.Title.Render("Configuration")), "\x1b[1m") {
		t.Fatal("title unexpectedly renders bold")
	}
	if got := theme.Focused.SelectedPrefix.Value(); got != "[x] " {
		t.Fatalf("selected Huh checkbox prefix = %q", got)
	}
	if got := theme.Focused.UnselectedPrefix.Value(); got != "[ ] " {
		t.Fatalf("unselected Huh checkbox prefix = %q", got)
	}
}
