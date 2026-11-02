package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

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
	return huh.NewForm(huh.NewGroup(fields...)).
		WithInput(input).
		WithOutput(output).
		RunWithContext(ctx)
}

func terminalNote(ctx context.Context, input io.Reader, output io.Writer, title, body string) error {
	return terminalForm(ctx, input, output, huh.NewNote().
		Title(title).
		Description(strings.TrimSpace(body)).
		Height(16))
}

func terminalConfirm(ctx context.Context, input io.Reader, output io.Writer, title, description string) (bool, error) {
	confirmed := false
	err := terminalForm(ctx, input, output, huh.NewConfirm().
		Title(title).
		Description(description).
		Affirmative("Yes").
		Negative("Cancel").
		Value(&confirmed))
	if errors.Is(err, huh.ErrUserAborted) {
		return false, nil
	}
	return confirmed, err
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
	selected := append([]string(nil), initial...)
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
		Description("Space toggles a harness. AgentMux will create only missing global source files.").
		Options(choices...).
		Value(&selected))
	if err != nil {
		return nil, fmt.Errorf("harness setup cancelled: %w", err)
	}
	return selected, nil
}
