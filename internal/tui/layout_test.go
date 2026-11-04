package tui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestLongNavigationPinsBackDuringScrollingAndFiltering(t *testing.T) {
	for _, width := range []int{45, 90} {
		options := append(longNavigationOptions(25), huh.NewOption("Back", "back"))
		var selected string
		field := huh.NewSelect[string]().Title("Skills").Options(options...).Value(&selected)
		view := &navigationField{Select: field, options: options}
		form, group := newNaturalForm(strings.NewReader(""), io.Discard, view)
		_ = form.Init()
		resizeNaturalForm(form, group, []huh.Field{view}, width, 24)
		for step := 0; step < 25; step++ {
			text := ansi.Strip(view.View())
			lines := strings.Split(text, "\n")
			if len(lines) != 11 || !strings.HasSuffix(strings.TrimSpace(lines[10]), "Back") || strings.Count(text, "Back") != 1 {
				t.Fatalf("Back moved while scrolling at %d:\n%s", step, text)
			}
			if !strings.Contains(text, fmt.Sprintf("CHOICE_%02d", step)) || strings.Count(text, "CHOICE_") != 9 {
				t.Fatalf("scrolling lost the cursor or left empty rows at %d:\n%s", step, text)
			}
			_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
		}
		if selected != "back" || !strings.Contains(ansi.Strip(view.View()), "> Back") {
			t.Fatal("Down did not reach the pinned Back action")
		}
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyHome})
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
		for _, letter := range "23" {
			_, _ = form.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{letter}})
		}
		text := ansi.Strip(view.View())
		if !strings.Contains(text, "CHOICE_23") || strings.Contains(text, "CHOICE_22") || !strings.Contains(text, "Back") {
			t.Fatalf("filter lost results or Back:\n%s", text)
		}
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyTab})
		if selected != "back" || !strings.Contains(ansi.Strip(view.View()), "> Back") {
			t.Fatal("Tab did not reach Back from an active filter")
		}
		_, cmd := form.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if cmd == nil || selected != "back" {
			t.Fatal("pinned Back did not submit")
		}
	}
}

func TestLongCheckboxMenuPinsBackWhileFiltering(t *testing.T) {
	options := longNavigationOptions(25)
	field := huh.NewMultiSelect[string]().Title("Skills").Options(options...)
	view := &backMultiSelect{MultiSelect: field, options: options}
	form, group := newNaturalForm(strings.NewReader(""), io.Discard, view)
	_ = form.Init()
	resizeNaturalForm(form, group, []huh.Field{view}, 90, 24)
	for range 20 {
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if !strings.Contains(view.View(), "Back") {
		t.Fatal("checkbox Back disappeared while filtering")
	}
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !view.back || !strings.Contains(ansi.Strip(view.View()), "> Back") {
		t.Fatal("checkbox Back shortcut is unavailable while filtering")
	}
}

func TestPinnedNavigationKeepsWrappedSelectionAndSuffixLabels(t *testing.T) {
	options := append(longNavigationOptions(15), huh.NewOption("Go Back", "item:go-back"), huh.NewOption("Back", "back"))
	field := huh.NewSelect[string]().Title("Skills").Options(options...)
	view := &navigationField{Select: field, options: options}
	form, group := newNaturalForm(strings.NewReader(""), io.Discard, view)
	_ = form.Init()
	resizeNaturalForm(form, group, []huh.Field{view}, 27, 24)
	for range 10 {
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	text := ansi.Strip(view.View())
	cursor := strings.Index(text, "> CHOICE_10")
	if cursor < 0 || !strings.Contains(text[cursor:], "descriptive action") || !strings.Contains(text[cursor:], "label") {
		t.Fatalf("wrapped selected option was cut off:\n%s", text)
	}
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	for _, letter := range "Go" {
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{letter}})
	}
	text = ansi.Strip(view.View())
	if !strings.Contains(text, "Go Back") || strings.Count(text, "Back") != 2 {
		t.Fatalf("filter removed an item whose label ends in Back:\n%s", text)
	}
}

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
	if !strings.Contains(view.View(), "Back") {
		t.Fatal("Back disappeared while filtering")
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

func TestInitImportMultiSelectScrollsNativelyAfterTenRows(t *testing.T) {
	options := longNavigationOptions(25)
	for i := range options {
		options[i] = options[i].Selected(true)
	}
	var selected []string
	field := huh.NewMultiSelect[string]().Title("Import existing MCP servers").Description("Choose definitions to import.").Options(options...).Value(&selected)
	form, group := newNaturalForm(strings.NewReader(""), io.Discard, field)
	_ = form.Init()
	for _, height := range []int{24, 12, 24} {
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyHome})
		var fallback bool
		msg := updateNaturalWindowSize(form, group, []huh.Field{field}, tea.WindowSizeMsg{Width: 90, Height: height}, &fallback)
		if _, ok := msg.(tea.WindowSizeMsg); !ok || fallback {
			t.Fatalf("long init list fell back instead of scrolling at height %d", height)
		}
		_, _ = form.Update(msg)
		if lipgloss.Height(form.View()) >= height || !strings.Contains(form.View(), "CHOICE_00") || strings.Contains(form.View(), "CHOICE_10") {
			t.Fatalf("init viewport does not fit its terminal:\n%s", form.View())
		}
		if height == 24 && !strings.Contains(form.View(), "CHOICE_09") {
			t.Fatal("fewer than ten rows in a large terminal")
		}
		for range 24 {
			_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
		}
		if !strings.Contains(form.View(), "CHOICE_24") || len(selected) != 25 {
			t.Fatal("scrolling lost selections or made the last import unreachable")
		}
	}
}
