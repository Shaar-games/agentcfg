package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

func longNavigationOptions(count int) []huh.Option[string] {
	options := make([]huh.Option[string], 0, count)
	for i := 0; i < count; i++ {
		options = append(options, huh.NewOption(fmt.Sprintf("CHOICE_%02d - a descriptive action label", i), fmt.Sprintf("id-%02d", i)))
	}
	return options
}

// Exercise Huh's real Form.Update and Select.View path rather than inspecting
// AST text for a missing `.Height()` call. The form height must stay natural
// even when a WindowSizeMsg advertises fewer rows than the menu needs.
func TestNaturalHuhFormKeepsAllLongSelectRowsWhenTerminalSmallAndResized(t *testing.T) {
	options := longNavigationOptions(25)
	var selection string
	field := huh.NewSelect[string]().Title("Many actions").Description("Enter immediately activates highlighted row").Options(options...).Value(&selection)
	form, group := newNaturalForm(strings.NewReader(""), io.Discard, field)
	_ = form.Init()
	for _, size := range []tea.WindowSizeMsg{{Width: 100, Height: 7}, {Width: 32, Height: 5}, {Width: 86, Height: 16}, {Width: 40, Height: 4}} {
		resizeNaturalForm(form, group, []huh.Field{field}, size.Width)
		_, _ = form.Update(size)
		view := form.View()
		for i := range options {
			if !strings.Contains(view, fmt.Sprintf("CHOICE_%02d", i)) {
				t.Fatalf("Huh clipped action %02d after resize %+v: visible lines %d:\n%s", i, size, lipgloss.Height(view), view)
			}
		}
		if lipgloss.Height(view) <= size.Height {
			t.Fatalf("test setup should exceed tiny terminal height; %+v yielded %d lines", size, lipgloss.Height(view))
		}
	}
	// Navigation to the final item must work without any inner viewport offset.
	for i := 0; i < len(options)-1; i++ {
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if selection != "id-24" || !strings.Contains(form.View(), "CHOICE_24") {
		t.Fatalf("down navigation did not reach final action: selected=%q", selection)
	}
}

func TestHuhDefaultFormClipsLongSelectWhileNaturalFormDoesNot(t *testing.T) {
	options := longNavigationOptions(22)
	var defaultSelected string
	defaultField := huh.NewSelect[string]().Title("Default").Options(options...).Value(&defaultSelected)
	defaultForm := huh.NewForm(huh.NewGroup(defaultField)).WithTheme(Theme())
	_, _ = defaultForm.Update(tea.WindowSizeMsg{Width: 75, Height: 7})
	if strings.Contains(defaultForm.View(), "CHOICE_21") {
		t.Fatal("Huh default did not reproduce the source of scroll/clipping; inspect the regression test")
	}
	var selected string
	field := huh.NewSelect[string]().Title("No viewport clipping").Options(options...).Value(&selected)
	form, group := newNaturalForm(strings.NewReader(""), io.Discard, field)
	resizeNaturalForm(form, group, []huh.Field{field}, 75)
	_, _ = form.Update(tea.WindowSizeMsg{Width: 75, Height: 7})
	if !strings.Contains(form.View(), "CHOICE_21") {
		t.Fatal("natural form still clips long select")
	}
}

func TestNaturalHuhFormFiltersAndConfirmsLastSelectedAction(t *testing.T) {
	options := longNavigationOptions(24)
	var selection string
	field := huh.NewSelect[string]().Title("Filter actions").Options(options...).Value(&selection)
	form, group := newNaturalForm(strings.NewReader(""), io.Discard, field)
	_ = form.Init()
	resizeNaturalForm(form, group, []huh.Field{field}, 44)
	_, _ = form.Update(tea.WindowSizeMsg{Width: 44, Height: 5})
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	for _, letter := range "23" {
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{letter}})
	}
	if !strings.Contains(form.View(), "CHOICE_23") || strings.Contains(form.View(), "CHOICE_22") {
		t.Fatalf("native Huh filtering failed with many choices:\n%s", form.View())
	}
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeyEnter})    // Close Huh's filter.
	_, cmd := form.Update(tea.KeyMsg{Type: tea.KeyEnter}) // Activate selected row.
	if selection != "id-23" {
		t.Fatalf("Enter did not choose last filtered option: %q", selection)
	}
	if cmd == nil {
		t.Fatal("native Huh Enter did not submit an action")
	}
}

func TestNotesUseNaturalHeightWithoutPadOrExtraNewline(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var output bytes.Buffer
	input := terminalByteReader{strings.NewReader("")}
	if err := RunForm(context.Background(), input, &output,
		huh.NewNote().Title("A short note").Description("One concise sentence.")); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "A short note") || !strings.Contains(text, "One concise sentence.") {
		t.Fatalf("note was hidden: %q", text)
	}
	if strings.Count(text, "\n") > 3 || strings.Contains(text, "\n\n\n") || !strings.HasSuffix(text, "\n") {
		t.Fatalf("note added blank height beyond Huh's own accessible output: %q", text)
	}
}

func TestNativeResizeIntoOverflowFallsBackToCompleteHuhAccessibleList(t *testing.T) {
	options := longNavigationOptions(24)
	var selected string
	var out bytes.Buffer
	field := huh.NewSelect[string]().Title("Resizing menu").Options(options...).Value(&selected)
	var fallback bool
	form, group := newNaturalFormWithOverflow(strings.NewReader("\n"), &out, &fallback, field)
	_ = form.Init()
	for range len(options) - 1 {
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if selected != "id-23" {
		t.Fatalf("failed to highlight the last option before resize: %q", selected)
	}
	msg := updateNaturalWindowSize(form, group, []huh.Field{field}, tea.WindowSizeMsg{Width: 33, Height: 5}, &fallback)
	if _, ok := msg.(tea.QuitMsg); !ok || !fallback {
		t.Fatalf("tiny terminal must quit native rendering before it clips or scrolls: %T, flag=%t", msg, fallback)
	}
	// The same selected Huh Select runs in numbered accessible mode and keeps
	// its highlighted value. The Enter default in accessible mode confirms it.
	form.WithAccessible(true)
	if err := form.RunWithContext(context.Background()); err != nil || selected != "id-23" {
		t.Fatalf("selected option was lost during Huh native -> accessible transition: %q, %v", selected, err)
	}
	if !strings.Contains(out.String(), "24. CHOICE_23") {
		t.Fatalf("accessible fallback did not show every option: %s", out.String())
	}
	var noFallback bool
	msg = updateNaturalWindowSize(form, group, []huh.Field{field}, tea.WindowSizeMsg{Width: 120, Height: 100}, &noFallback)
	if size, ok := msg.(tea.WindowSizeMsg); !ok || size.Height != 100 || noFallback {
		t.Fatalf("a large terminal should keep native Huh layout unchanged: %T %v", msg, noFallback)
	}
}

func TestHuhAccessibleRunListConsumesOrdinaryReaderAcrossScreens(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := t.TempDir()
	var rendered bytes.Buffer
	// This is deliberately a plain strings.Reader, not terminalByteReader.
	// Without guarded reads, Huh's first scanner consumes all four answers.
	if err := RunList(context.Background(), root, strings.NewReader("1\n1\n8\n6\n"), &rendered); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"agentcfg", "Inventory", "Harness: Claude Code", "Back to inventory"} {
		if !strings.Contains(rendered.String(), label) {
			t.Fatalf("multi-screen Huh accessible navigation lost %q:\n%s", label, rendered.String())
		}
	}
	if strings.Contains(rendered.String(), "\n\n\n") {
		t.Fatalf("accessible transitions accumulated blank lines:\n%q", rendered.String())
	}
}

func TestHuhAccessibleEOFDoesNotSilentlySelectDefault(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var selected string
	var rendered bytes.Buffer
	err := RunForm(context.Background(), strings.NewReader(""), &rendered,
		huh.NewSelect[string]().Title("Pick an action").Options(
			huh.NewOption("Delete", "delete"),
			huh.NewOption("Back", "back"),
		).Value(&selected))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF instead of silently accepting Huh default, got %v", err)
	}
	root := t.TempDir()
	if err := RunMenu(context.Background(), root, strings.NewReader(""), &bytes.Buffer{}); !errors.Is(err, io.EOF) {
		t.Fatalf("menu without input should terminate on EOF, got %v", err)
	}
}

type refusingOutput struct{ err error }

func (w refusingOutput) Write([]byte) (int, error) { return 0, w.err }

func TestHuhAccessibleWriteErrorsAndCancellationAreNotApproved(t *testing.T) {
	t.Setenv("TERM", "dumb")
	outputErr := errors.New("disk/terminal writer unavailable")
	var selected string
	err := RunForm(context.Background(), strings.NewReader("1\n"), refusingOutput{outputErr},
		huh.NewSelect[string]().Title("Select").Options(huh.NewOption("Action", "action")).Value(&selected))
	if !errors.Is(err, outputErr) {
		t.Fatalf("output failure not returned by Huh wrapper: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	err = RunForm(ctx, strings.NewReader("1\n"), &output,
		huh.NewSelect[string]().Title("Select").Options(huh.NewOption("Action", "action")).Value(&selected))
	if !errors.Is(err, context.Canceled) || output.Len() != 0 {
		t.Fatalf("canceled form unexpectedly continued or printed: %v output=%q", err, output.String())
	}
}

func TestNativeHuhAltScreenRestoresPrimarySurfaceOnEnterAndAbort(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	for _, tc := range []struct {
		name  string
		input byte
		abort bool
	}{
		{"enter", '\r', false},
		{"ctrl-c", 0x03, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			input, userInput := io.Pipe()
			defer input.Close()
			defer userInput.Close()
			var output bytes.Buffer
			var selection string
			field := huh.NewSelect[string]().Title("Native menu").Options(
				huh.NewOption("Continue", "continue"),
				huh.NewOption("Back", "back"),
			).Value(&selection)
			var overflow bool // Native path enables Tea's alternate-screen option.
			form, _ := newNaturalFormWithOverflow(input, &output, &overflow, field)
			form.WithAccessible(false)
			result := make(chan error, 1)
			go func() { result <- form.RunWithContext(ctx) }()
			go func() { _, _ = userInput.Write([]byte{tc.input}) }()
			select {
			case err := <-result:
				if tc.abort && !isCancelled(err) {
					t.Fatalf("Ctrl+C must report normal Huh cancellation: %v", err)
				}
				if !tc.abort && err != nil {
					t.Fatalf("Enter must return normally: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("native Huh form failed to complete after keyboard input")
			}
			contents := output.String()
			if !strings.Contains(contents, "\x1b[?1049h") || !strings.Contains(contents, "\x1b[?1049l") {
				t.Fatalf("native screen was not restored on %s: %q", tc.name, contents)
			}
			if strings.LastIndex(contents, "\x1b[?1049l") < strings.Index(contents, "\x1b[?1049h") {
				t.Fatalf("alternate screen closed before opening on %s: %q", tc.name, contents)
			}
		})
	}
}

func TestNonTerminalStreamsUsePlainAccessibleHuhRegardlessOfTERM(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("AGENTCFG_ACCESSIBLE", "")
	root := t.TempDir()
	var output bytes.Buffer
	if err := RunList(context.Background(), root, strings.NewReader("1\n1\n8\n6\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Harness: Claude Code") || !strings.Contains(output.String(), "Enter a number") {
		t.Fatalf("nonTTY output with TERM=xterm did not use accessible menu: %s", output.String())
	}
	if strings.Contains(output.String(), "\x1b") {
		t.Fatalf("nonTTY accessible output contains ANSI sequences: %q", output.String())
	}
}

func TestNilInputCannotReadProcessStdinOrApproveDefault(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	var output bytes.Buffer
	var selected string
	err := RunForm(context.Background(), nil, &output,
		huh.NewSelect[string]().Title("Select").Options(huh.NewOption("First", "first")).Value(&selected))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("nil input should fail as EOF instead of reading os.Stdin or selecting first: %v", err)
	}
}

func TestAccessibleOutputStripsANSIEvenWhenSplitAcrossWrites(t *testing.T) {
	var output bytes.Buffer
	plain := &plainAccessibleWriter{w: &output}
	for _, part := range []string{
		"\x1b[38;2;217;", "225;234mStyled title\x1b[", "m\n",
		"\x1b]0;window title", "\x1b\\", "Readable option\n",
		"\x1bPignored", "\x1b\\", "Final — Être sûr, Û\n",
	} {
		if _, err := plain.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if got := output.String(); got != "Styled title\nReadable option\nFinal — Être sûr, Û\n" {
		t.Fatalf("accessible writer left ANSI/style/OSC/DCS controls in output: %q", got)
	}
}

func TestNaturalFormPreservesTrueMultiSelectAcrossResizes(t *testing.T) {
	options := make([]huh.Option[string], 0, 20)
	for i := range 20 {
		id := fmt.Sprintf("harness-%02d", i)
		options = append(options, huh.NewOption(id+"  ·  multi-value availability", id).Selected(i == 0 || i == 19))
	}
	var chosen []string
	field := huh.NewMultiSelect[string]().Title("Harness availability").Options(options...).Value(&chosen)
	form, group := newNaturalForm(strings.NewReader(""), io.Discard, field)
	_ = form.Init()
	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 10}, {Width: 31, Height: 5}, {Width: 75, Height: 30}} {
		resizeNaturalForm(form, group, []huh.Field{field}, size.Width)
		_, _ = form.Update(size)
	}
	if !slices.Contains(chosen, "harness-00") || !slices.Contains(chosen, "harness-19") {
		t.Fatalf("initial multi-select activations were lost after resize: %v", chosen)
	}
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeySpace})
	if !slices.Contains(chosen, "harness-01") {
		t.Fatalf("multi-select toggling stopped working after resize: %v", chosen)
	}
}
