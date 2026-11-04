package tui

import (
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
)

// Warnings are informational, never a second confirmation. Keep redirected
// output and NO_COLOR plain; use the shared amber palette on native terminals.
func WriteWarning(output io.Writer, message string) error {
	if output == nil {
		return nil
	}
	line := "Warning: " + message
	if file, ok := output.(*os.File); ok && term.IsTerminal(file.Fd()) && !navigationPlainMode() {
		line = lipgloss.NewRenderer(output).NewStyle().Foreground(navWarnColor).Render(line)
	}
	_, err := fmt.Fprintln(output, line)
	return err
}
