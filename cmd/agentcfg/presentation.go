package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"agentcfg/internal/tui"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
)

// Huh is exclusively a human-facing terminal presentation layer. MCP stdio,
// JSON/piped output, and automation must never encounter terminal prompts or
// ANSI control sequences. The caller can also force plain output with --plain.
func terminalPresentation(input io.Reader, output io.Writer, plain bool) bool {
	if plain {
		return false
	}
	in, okIn := input.(*os.File)
	out, okOut := output.(*os.File)
	return okIn && okOut && term.IsTerminal(in.Fd()) && term.IsTerminal(out.Fd())
}

func terminalForm(ctx context.Context, input io.Reader, output io.Writer, fields ...huh.Field) error {
	return tui.RunForm(ctx, input, output, fields...)
}

func terminalNote(ctx context.Context, input io.Reader, output io.Writer, title, body string) error {
	return terminalForm(ctx, input, output, huh.NewNote().
		Title(title).
		Description(strings.TrimSpace(body)))
}

func terminalConfirm(ctx context.Context, input io.Reader, output io.Writer, title, description string) (bool, error) {
	confirmed := false

	err := terminalForm(ctx, input, output, terminalConfirmation(title, description, &confirmed))
	if errors.Is(err, huh.ErrUserAborted) {
		return false, nil
	}
	return confirmed, err
}

// The shared field shows the action once, without a generic extra question.
func terminalConfirmation(title, description string, confirmed *bool) huh.Field {
	return tui.Confirmation(strings.TrimSpace(title), strings.TrimSpace(description), confirmed)
}

func terminalHarnessSelection(ctx context.Context, input io.Reader, output io.Writer, initial []string) ([]string, error) {
	var selected []string
	choices := tui.HarnessOptions(nil, initial)
	err := terminalForm(ctx, input, output, huh.NewMultiSelect[string]().
		Title("Select your AI harnesses").
		Options(choices...).
		Value(&selected))
	if err != nil {
		return nil, fmt.Errorf("harness setup cancelled: %w", err)
	}
	return selected, nil
}
