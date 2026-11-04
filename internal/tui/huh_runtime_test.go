package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// Exercise actual Huh updates and rendering: exactly ten options initially,
// then native scrolling must make the last option reachable.
func TestMenuShowsTenOptionsBeforeScrolling(t *testing.T) {
	for _, colored := range []bool{false, true} {
		var selection string
		field := huh.NewSelect[string]().Title("Many actions").Description("Select an action").Options(longNavigationOptions(25)...).Value(&selection)
		var rendered huh.Field = field
		if colored {
			rendered = &navigationField{Select: field, options: longNavigationOptions(25), renderer: lipgloss.NewRenderer(io.Discard)}
		}
		form, group := newNaturalForm(strings.NewReader(""), io.Discard, rendered)
		_ = form.Init()
		for _, width := range []int{100, 60, 85} {
			resizeNaturalForm(form, group, []huh.Field{rendered}, width)
			_, _ = form.Update(tea.WindowSizeMsg{Width: width, Height: 30})
			view := form.View()
			for i := 0; i < 10; i++ {
				if !strings.Contains(view, fmt.Sprintf("CHOICE_%02d", i)) {
					t.Fatalf("missing initial option %d: %s", i, view)
				}
			}
			if strings.Contains(view, "CHOICE_10") {
				t.Fatalf("more than ten options: %s", view)
			}
		}
		for range 24 {
			_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
		}
		if selection != "id-24" || !strings.Contains(form.View(), "CHOICE_24") {
			t.Fatalf("last option unreachable: %s", form.View())
		}
	}
}

func TestShortMultiSelectStartsAtTopWithLastOptionChecked(t *testing.T) {
	for _, count := range []int{6, 10, 20} {
		options := longNavigationOptions(count)
		options[count-1] = options[count-1].Selected(true)
		var chosen []string
		field := huh.NewMultiSelect[string]().Title("Harnesses").Options(options...).Value(&chosen)
		form, group := newNaturalForm(strings.NewReader(""), io.Discard, field)
		_ = form.Init()
		for _, width := range []int{80, 45, 90} {
			resizeNaturalForm(form, group, []huh.Field{field}, width)
			_, _ = form.Update(tea.WindowSizeMsg{Width: width, Height: 30})
			for i := 0; i < min(count, 10); i++ {
				if !strings.Contains(form.View(), fmt.Sprintf("CHOICE_%02d", i)) {
					t.Fatalf("initial option %d hidden for %d choices: %s", i, count, form.View())
				}
			}
		}
		if count <= 10 {
			for range count - 1 {
				_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
				for i := range count {
					if !strings.Contains(form.View(), fmt.Sprintf("CHOICE_%02d", i)) {
						t.Fatalf("short checkbox list scrolls: %s", form.View())
					}
				}
			}
		}
		if !slices.Contains(chosen, fmt.Sprintf("id-%02d", count-1)) {
			t.Fatal("prechecked state lost")
		}
	}
}

func TestShortSelectNeverScrollsAndPreservesInitialValue(t *testing.T) {
	for _, count := range []int{2, 6, 10} {
		options := longNavigationOptions(count)
		selected := fmt.Sprintf("id-%02d", count-1)
		field := huh.NewSelect[string]().Title("Sharing").Options(options...).Value(&selected)
		form, group := newNaturalForm(strings.NewReader(""), io.Discard, field)
		_ = form.Init()
		resizeNaturalForm(form, group, []huh.Field{field}, 100)
		if selected != fmt.Sprintf("id-%02d", count-1) {
			t.Fatal("initial value changed")
		}
		for range count * 2 {
			for i := range count {
				if !strings.Contains(form.View(), fmt.Sprintf("CHOICE_%02d", i)) {
					t.Fatalf("short list scrolls at %s: %s", selected, form.View())
				}
			}
			_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
		}
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
	if form.View() != "" || form.State != huh.StateAborted {
		t.Fatal("resize handoff must clear the inline form instead of leaving its footer behind")
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

func TestResizeDoesNotReopenAnAlreadyCompletedOrAbortedForm(t *testing.T) {
	for _, state := range []huh.FormState{huh.StateCompleted, huh.StateAborted} {
		field := huh.NewSelect[string]().Options(longNavigationOptions(24)...)
		form, group := newNaturalForm(strings.NewReader(""), io.Discard, field)
		form.State = state
		var fallback bool
		size := tea.WindowSizeMsg{Width: 40, Height: 3}
		msg := updateNaturalWindowSize(form, group, []huh.Field{field}, size, &fallback)
		if msg != size || fallback || form.State != state {
			t.Fatalf("resize reopened finished form: state=%v fallback=%v msg=%v", form.State, fallback, msg)
		}
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
	for _, label := range []string{"agentcfg", "Inventory", "Harness: Claude Code", "Back"} {
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

// Wait for a real rendered frame before sending input. Sending Enter/Ctrl+C
// immediately can finish before the renderer paints, hiding cleanup defects.
type inlineFrameOutput struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	painted chan struct{}
	once    sync.Once
}

func (w *inlineFrameOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buffer.Write(p)
	if strings.Contains(w.buffer.String(), "NATIVE_MENU") {
		w.once.Do(func() { close(w.painted) })
	}
	return n, err
}

func (w *inlineFrameOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

// This bounded trace check only needs vertical cursor commands for the short,
// non-wrapping fixture. A menu must leave the cursor at its starting row, never
// move above it, or switch/clear the whole screen to hide an inline layout bug.
func assertInlineSurfaceRestored(t *testing.T, contents string) {
	t.Helper()
	for _, forbidden := range []string{"\x1b[?1049", "\x1b[?1047", "\x1b[?47", "\x1b[2J", "\x1b[3J", "\x1b[H", "\x1b[1;1H"} {
		if strings.Contains(contents, forbidden) {
			t.Fatalf("inline menu switched or cleared the whole terminal (%q): %q", forbidden, contents)
		}
	}
	commands := regexp.MustCompile("\x1b\\[([0-9;?]*)([A-Za-z])|\n")
	row, highest := 0, 0
	for _, command := range commands.FindAllStringSubmatch(contents, -1) {
		if command[0] == "\n" {
			row++
		} else {
			count := 1
			if value, err := strconv.Atoi(command[1]); err == nil && value > 0 {
				count = value
			}
			switch command[2] {
			case "A", "F":
				row -= count
			case "B", "E":
				row += count
			case "H", "f", "d":
				t.Fatalf("inline menu used absolute vertical positioning: %q", command[0])
			}
		}
		if row < 0 {
			t.Fatalf("menu cleanup moved into pre-existing terminal output: %q", contents)
		}
		highest = max(highest, row)
	}
	if highest < 2 {
		t.Fatalf("test did not paint a multi-line menu before quitting: %q", contents)
	}
	if row != 0 {
		t.Fatalf("menu left %d extra rows instead of restoring its own starting row: %q", row, contents)
	}
}

func TestNativeHuhStaysInlineAndClearsOwnRowsOnEnterAndAbort(t *testing.T) {
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
			output := &inlineFrameOutput{painted: make(chan struct{})}
			var selection string
			field := huh.NewSelect[string]().Title("NATIVE_MENU").Options(
				huh.NewOption("Continue", "continue"),
				huh.NewOption("Back", "back"),
			).Value(&selection)
			var overflow bool
			form, _ := newNaturalFormWithOverflow(input, output, &overflow, field)
			form.WithAccessible(false)
			result := make(chan error, 1)
			go func() { result <- form.RunWithContext(ctx) }()
			select {
			case <-output.painted:
			case err := <-result:
				t.Fatalf("form stopped before rendering: %v", err)
			case <-ctx.Done():
				t.Fatal("native form did not render its first frame")
			}
			go func() { _, _ = userInput.Write([]byte{tc.input}) }()
			select {
			case err := <-result:
				if tc.abort && !isCancelled(err) {
					t.Fatalf("Ctrl+C must report normal Huh cancellation: %v", err)
				}
				if !tc.abort && err != nil {
					t.Fatalf("Enter must return normally: %v", err)
				}
				if tc.abort && form.State != huh.StateAborted {
					t.Fatal("cleanup changed cancellation into a successful submission")
				}
			case <-ctx.Done():
				t.Fatal("native Huh form failed to complete after keyboard input")
			}
			assertInlineSurfaceRestored(t, output.String())
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
