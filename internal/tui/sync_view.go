package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// syncPreviewLines returns only the operations and warnings supplied by
// syncer.BuildPlan. The preview never changes configuration by rendering.
func (m Model) syncPreviewLines() []string {
	if m.syncModal.loading {
		return []string{"Reading canonical settings and native user-scope targets…"}
	}
	if m.syncModal.err != nil {
		return []string{"Cannot prepare or apply sync:", m.syncModal.err.Error(), "Fix the problem and press r to preview again."}
	}
	plan := m.syncModal.plan
	if plan == nil {
		return []string{"No sync plan available. Press r to preview again."}
	}
	if len(plan.Changes) == 0 && len(plan.Warnings) == 0 {
		return []string{"User-scope targets are up to date. No files need updating."}
	}
	lines := make([]string, 0, len(plan.Changes)+len(plan.Warnings)+5)
	lines = append(lines, fmt.Sprintf("Planned changes: %d", len(plan.Changes)))
	if len(plan.Changes) == 0 {
		lines = append(lines, "Nothing to install, update or remove.")
	}
	for _, change := range plan.Changes {
		path := change.Path
		if m.root != "" {
			if relative, err := filepath.Rel(m.root, path); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				path = "~/" + filepath.ToSlash(relative)
			}
		}
		lines = append(lines, "• "+change.Action+": "+path)
	}
	if len(plan.Warnings) != 0 {
		lines = append(lines, "", fmt.Sprintf("Warnings: %d", len(plan.Warnings)))
		for _, warning := range plan.Warnings {
			lines = append(lines, "! "+warning)
		}
	}
	return lines
}

func (m Model) syncModalView() string {
	width, height := max(38, m.width), max(13, m.height)
	innerWidth := max(25, min(76, width-10))
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(blue).
		Padding(0, 1).Width(innerWidth + 2)
	header := []string{
		lipgloss.NewStyle().Foreground(blue).Bold(true).Render("USER HARNESS SYNC"),
		lipgloss.NewStyle().Foreground(muted).Render("Read-only preview · source SKILL.md stays unchanged"),
		lipgloss.NewStyle().Foreground(muted).Render("Skills: symbolic links preferred, exact copy if unavailable"),
		"",
	}
	preview := m.syncPreviewLines()
	maxPreview := max(1, height-10)
	start := min(m.syncModal.scroll, max(0, len(preview)-maxPreview))
	end := min(len(preview), start+maxPreview)
	for _, line := range preview[start:end] {
		header = append(header, lipgloss.NewStyle().Foreground(white).Render(widthLimit(safeText(line), innerWidth)))
	}
	if len(preview) > maxPreview {
		header = append(header, lipgloss.NewStyle().Foreground(muted).Render(fmt.Sprintf("Lines %d–%d of %d", start+1, end, len(preview))))
	}
	header = append(header, "")
	command := "↑↓ scroll  ·  r refresh preview  ·  Esc close"
	switch {
	case m.syncModal.applying:
		command = "Applying confirmed changes…"
	case m.syncModal.loading:
		command = "Preparing preview…  ·  Esc cancel"
	case m.syncModal.err != nil:
		command = "r retry preview  ·  Esc close"
	case m.syncModal.plan != nil && len(m.syncModal.plan.Changes) > 0:
		command = "y apply changes  ·  ↑↓ scroll  ·  r rebuild  ·  Esc cancel"
	}
	header = append(header, lipgloss.NewStyle().Foreground(yellow).Render(widthLimit(command, innerWidth)))
	panel := style.Render(strings.Join(header, "\n"))
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}
