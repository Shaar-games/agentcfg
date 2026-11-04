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
)

func TestLongInitPreviewScrollsWithoutAdvancingUntilEnter(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&body, "Native warning %02d\n", i)
	}
	fields := scrollNotes([]huh.Field{huh.NewNote().Title("Native synchronization preview").Description(body.String())})
	form, group := newNaturalForm(strings.NewReader(""), io.Discard, fields...)
	_ = form.Init()
	for _, height := range []int{24, 10, 24} {
		var fallback bool
		msg := updateNaturalWindowSize(form, group, fields, tea.WindowSizeMsg{Width: 90, Height: height}, &fallback)
		if fallback {
			t.Fatal("long note fell back to a complete numbered/plain screen")
		}
		_, _ = form.Update(msg)
		if lipgloss.Height(form.View()) >= height || strings.Contains(form.View(), "Native warning 29") {
			t.Fatalf("preview does not fit: %s", form.View())
		}
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyEnd})
		if form.State != huh.StateNormal || !strings.Contains(form.View(), "Native warning 29") {
			t.Fatal("scrolling advanced or hid the end of the note")
		}
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyHome})
	}
	_, cmd := form.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter no longer advances the preview")
	}
	var output bytes.Buffer
	if err := RunForm(context.Background(), strings.NewReader(""), &output, huh.NewNote().Title("Preview").Description(body.String())); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Native warning 29") {
		t.Fatal("accessible notes lost content")
	}
}
