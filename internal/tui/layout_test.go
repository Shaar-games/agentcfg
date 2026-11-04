package tui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestBackOccupiesTenthRowWithoutSelectableSpacers(t *testing.T) {
	for _, count := range []int{0, 1, 2, 8, 9} {
		for _, colored := range []bool{false, true} {
			options := append(longNavigationOptions(count), huh.NewOption("Back", "back"))
			var selected string
			field := huh.NewSelect[string]().Title("Menu").Description("Choose an item").Options(options...).Value(&selected)
			view := &navigationField{Select: field, options: options}
			if colored {
				view.renderer = lipgloss.NewRenderer(io.Discard)
				view.renderer.SetColorProfile(termenv.TrueColor)
			}
			form, group := newNaturalForm(strings.NewReader(""), io.Discard, view)
			_ = form.Init()
			for _, width := range []int{90, 60, 100} {
				resizeNaturalForm(form, group, []huh.Field{view}, width)
				_, _ = form.Update(tea.WindowSizeMsg{Width: width, Height: 24})
				for step := 0; step <= count; step++ {
					lines := strings.Split(ansi.Strip(view.View()), "\n")
					if len(lines) != 12 || !strings.HasSuffix(strings.TrimSpace(lines[11]), "Back") {
						t.Fatalf("count=%d step=%d: Back is not in option row ten:\n%s", count, step, view.View())
					}
					if step < count {
						_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
					}
				}
				if selected != "back" {
					t.Fatalf("padding added keyboard steps: %q", selected)
				}
				_, _ = form.Update(tea.KeyMsg{Type: tea.KeyHome})
			}
			for _, option := range options {
				if strings.ContainsAny(option.Key, "\r\n\x1b") {
					t.Fatal("visual padding leaked into searchable labels")
				}
			}
			for range count {
				_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
			}
			_, cmd := form.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if selected != "back" || cmd == nil {
				t.Fatal("Back did not submit normally")
			}
		}
	}
}

func TestBackSpacingDoesNotAlterAccessiblePromptsOrFiltering(t *testing.T) {
	options := []huh.Option[string]{huh.NewOption("First", "first"), huh.NewOption("Back", "back")}
	var selected string
	field := huh.NewSelect[string]().Title("Menu").Options(options...).Value(&selected)
	view := &navigationField{Select: field, options: options}
	form, _ := newNaturalForm(strings.NewReader(""), io.Discard, view)
	_ = form.Init()
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("First")})
	if strings.Contains(view.View(), "Back") {
		t.Fatal("Back was injected into filtered results")
	}
	var output bytes.Buffer
	field = huh.NewSelect[string]().Title("Menu").Options(options...).Value(&selected)
	view = &navigationField{Select: field, options: options}
	if err := RunForm(context.Background(), strings.NewReader("2\n"), &output, view); err != nil {
		t.Fatal(err)
	}
	if selected != "back" || !strings.Contains(output.String(), "2. Back") || strings.Contains(output.String(), "\n\n\n") {
		t.Fatalf("accessible prompt contains visual spacer rows: %q", output.String())
	}
}

func TestBackSpacingShrinksToFitSmallTerminals(t *testing.T) {
	options := append(longNavigationOptions(2), huh.NewOption("Back", "back"))
	field := huh.NewSelect[string]().Title("Menu").Description("Choose an item").Options(options...)
	view := &navigationField{Select: field, options: options}
	form, group := newNaturalForm(strings.NewReader(""), io.Discard, view)
	_ = form.Init()
	for _, height := range []int{12, 18, 9, 24} {
		var overflow bool
		msg := updateNaturalWindowSize(form, group, []huh.Field{view}, tea.WindowSizeMsg{Width: 90, Height: height}, &overflow)
		if _, ok := msg.(tea.WindowSizeMsg); !ok || overflow {
			t.Fatalf("visual spacing alone caused an accessible fallback at height %d", height)
		}
		_, _ = form.Update(msg)
		if lipgloss.Height(form.View()) >= height || !strings.Contains(form.View(), "Back") {
			t.Fatalf("Back does not fit a %d-row terminal:\n%s", height, form.View())
		}
	}
}
