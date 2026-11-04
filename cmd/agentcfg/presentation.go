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

// All interactive mutations share the same explicit, default-negative prompt.
// Keep the operation and its consequences in the description, not in a second
// confirmation dialect that differs from the configuration menus.
func terminalConfirmation(title, description string, confirmed *bool) *huh.Confirm {
	return huh.NewConfirm().
		Title("Are you sure?").
		Description(strings.TrimSpace(title + "\n" + description)).
		Affirmative("Yes!").
		Negative("No.").
		Value(confirmed)
}

func terminalHarnessSelection(ctx context.Context, input io.Reader, output io.Writer, initial []string) ([]string, error) {
	options := []struct{ name, id string }{
		{"Claude Code", "claude-code"},
		{"Codex", "codex"},
		{"Cursor", "cursor"},
		{"OpenCode", "opencode"},
		{"Gemini CLI", "gemini"},
		{"Google Antigravity", "antigravity"},
	}
	var selected []string
	choices := make([]huh.Option[string], 0, len(options))
	for _, h := range options {
		active := false
		for _, existing := range initial {
			if h.id == existing {
				active = true
				break
			}
		}
		choices = append(choices, huh.NewOption(h.name, h.id).Selected(active))
	}
	err := terminalForm(ctx, input, output, huh.NewMultiSelect[string]().
		Title("Select user-level AI harnesses").
		Description("Space toggles a harness. agentcfg will create only missing global source files.").
		Options(choices...).
		Value(&selected))
	if err != nil {
		return nil, fmt.Errorf("harness setup cancelled: %w", err)
	}
	return selected, nil
}
