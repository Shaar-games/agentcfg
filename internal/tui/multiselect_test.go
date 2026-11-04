package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/config"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func TestHarnessBackIsBottomActionWithoutCheckboxOrSaving(t *testing.T) {
	options := longNavigationOptions(6)
	options[5] = options[5].Selected(true)
	var selected []string
	field := huh.NewMultiSelect[string]().Title("Harness availability").Options(options...).Value(&selected)
	view := &backMultiSelect{MultiSelect: field, options: options}
	form, group := newNaturalForm(strings.NewReader(""), io.Discard, view)
	form.CancelCmd = tea.Interrupt // Set by Huh's Run in a real native session.
	_ = form.Init()
	for _, height := range []int{24, 14, 24} {
		msg := updateNaturalWindowSize(form, group, []huh.Field{view}, tea.WindowSizeMsg{Width: 90, Height: height}, nil)
		_, _ = form.Update(msg)
		text := ansi.Strip(form.View())
		if !strings.Contains(text, "CHOICE_00") || !strings.Contains(text, "Back") || lipgloss.Height(text) >= height {
			t.Fatalf("Harnesses does not fit without scrolling at height %d:\n%s", height, text)
		}
	}
	lines := strings.Split(ansi.Strip(view.View()), "\n")
	if len(lines) != 11 || !strings.HasSuffix(strings.TrimSpace(lines[10]), "Back") {
		t.Fatalf("Back is not at row ten:\n%s", view.View())
	}
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeySpace})
	for range 6 {
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if !view.back || !strings.Contains(ansi.Strip(view.View()), "> Back") {
		t.Fatalf("six Down presses must reach Back directly:\n%s", view.View())
	}
	before := slices.Clone(selected)
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeySpace})
	if !slices.Equal(selected, before) || slices.Contains(selected, "back") {
		t.Fatal("Back was treated as a checkbox")
	}
	_, cmd := form.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if form.State != huh.StateAborted || cmd == nil || !view.backRequested || form.View() != "" {
		t.Fatal("Back must cancel and clear the form instead of submitting")
	}
}

func TestHarnessBackReturnsToCheckboxesAndEnterStillSubmits(t *testing.T) {
	options := longNavigationOptions(6)
	var selected []string
	field := huh.NewMultiSelect[string]().Title("Harnesses").Options(options...).Value(&selected)
	view := &backMultiSelect{MultiSelect: field, options: options}
	form, _ := newNaturalForm(strings.NewReader(""), io.Discard, view)
	_ = form.Init()
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !view.back {
		t.Fatal("Tab should focus Back")
	}
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeyUp})
	last, ok := field.Hovered()
	if view.back || !ok || last != "id-05" {
		t.Fatal("Up from Back must return to the last checkbox")
	}
	_, _ = form.Update(tea.KeyMsg{Type: tea.KeySpace})
	_, cmd := form.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || view.backRequested || !slices.Contains(selected, "id-05") {
		t.Fatal("normal checkbox submission changed")
	}
}

func TestHarnessAccessibleBackDiscardsPendingChangesAndReturnsToMain(t *testing.T) {
	t.Setenv("AGENTCFG_ACCESSIBLE", "1")
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["codex"]}`)
	before := mustRead(t, settingsFile(root))
	var output bytes.Buffer
	// Open Harnesses, toggle Cursor, Back, then Exit from configuration.
	if err := RunMenu(context.Background(), root, strings.NewReader("1\n3\nb\n6\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, mustRead(t, settingsFile(root))) {
		t.Fatal("Back wrote pending harness changes")
	}
	noBackupCreated(t, root)
	assertConfirmBehavior(t, output.String(), false)
	if strings.Count(output.String(), "Configuration") != 2 || !strings.Contains(output.String(), "Back without saving") {
		t.Fatalf("Back did not return to configuration:\n%s", output.String())
	}
}

func TestOverviewExitUsesBottomActionLayout(t *testing.T) {
	options := overviewNavigationOptions(&config.Config{}, 0)
	var selected string
	field := huh.NewSelect[string]().Title("Configuration").Options(options...).Value(&selected)
	view := (interfaceSession{}).navigationSelect(field, options)
	form, group := newNaturalForm(strings.NewReader(""), io.Discard, view)
	form.CancelCmd = tea.Interrupt // Set by Huh's Run in a real native session.
	_ = form.Init()
	resizeNaturalForm(form, group, []huh.Field{view}, 90, 24)
	for range 5 {
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	lines := strings.Split(ansi.Strip(view.View()), "\n")
	if selected != "exit" || len(lines) != 11 || !strings.HasSuffix(strings.TrimSpace(lines[10]), "> Exit") {
		t.Fatalf("Exit must be the tenth visual row and sixth real option:\n%s", view.View())
	}
}

func TestNativeHarnessBackCancelsAndClearsInlineForm(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	input, keys := io.Pipe()
	defer input.Close()
	defer keys.Close()
	output := &inlineFrameOutput{painted: make(chan struct{})}
	options := longNavigationOptions(6)
	var selected []string
	field := huh.NewMultiSelect[string]().Title("NATIVE_MENU").Options(options...).Value(&selected)
	view := &backMultiSelect{MultiSelect: field, options: options}
	form, _ := newNaturalForm(input, output, view)
	form.WithAccessible(false)
	result := make(chan error, 1)
	go func() { result <- form.RunWithContext(ctx) }()
	select {
	case <-output.painted:
	case err := <-result:
		t.Fatalf("form stopped before rendering: %v", err)
	case <-ctx.Done():
		t.Fatal("form did not paint")
	}
	go func() { _, _ = keys.Write([]byte{'\t', '\r'}) }()
	select {
	case err := <-result:
		if !errors.Is(err, huh.ErrUserAborted) {
			t.Fatalf("Back did not cancel the actual native session: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("native Back did not finish")
	}
	assertInlineSurfaceRestored(t, output.String())
}
