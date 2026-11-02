package tui

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"unicode"

	"agentmux/internal/config"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	navy   = lipgloss.Color("#13283F")
	blue   = lipgloss.Color("#79B7FF")
	white  = lipgloss.Color("#F2F6FF")
	muted  = lipgloss.Color("#8998AA")
	green  = lipgloss.Color("#79E0B0")
	yellow = lipgloss.Color("#FFD27F")
	red    = lipgloss.Color("#FF8994")
)

func (m Model) View() string {
	w := max(36, m.width-2)
	if m.showHelp {
		return m.helpView(w)
	}
	if m.syncModal.open {
		return m.syncModalView()
	}
	if m.skillModal.open {
		return m.skillModalView()
	}
	lines := []string{
		m.header(w),
		m.tabLine(w),
		lipgloss.NewStyle().Foreground(navy).Render(strings.Repeat("─", w)),
	}
	if m.loadError != nil {
		lines = append(lines, tone("Configuration error: "+m.loadError.Error(), red, w),
			plain("Press r to retry after correcting the configuration.", w))
	} else {
		lines = append(lines, m.sectionLabel(w))
		lines = append(lines, m.content(w)...)
	}
	if m.notice != "" {
		lines = append(lines, tone(m.notice, yellow, w))
	}
	footer := []string{
		lipgloss.NewStyle().Foreground(navy).Render(strings.Repeat("─", w)),
		plain("↑↓ select  ←→/Tab sections  Space action  a skill access  s sync preview  r refresh  ? help  q quit", w),
	}
	if len(lines)+len(footer) < m.height {
		for len(lines)+len(footer) < m.height {
			lines = append(lines, "")
		}
	} else if len(lines)+len(footer) > m.height {
		lines = lines[:max(0, m.height-len(footer))]
	}
	lines = append(lines, footer...)
	return strings.Join(lines, "\n")
}

func (m Model) header(w int) string {
	busy := "ready"
	color := green
	if m.loading || m.busy {
		busy, color = []string{"◐", "◓", "◑", "◒"}[m.spinner]+" loading", yellow
	} else if m.statusErr != nil {
		busy, color = "status error", red
	} else if m.status.Running {
		busy = fmt.Sprintf("broker PID %d", m.status.PID)
	} else {
		busy, color = "broker idle", muted
	}
	name := lipgloss.NewStyle().Foreground(blue).Bold(true).Render("◆ AGENTMUX")
	desc := ""
	if w >= 70 {
		desc = lipgloss.NewStyle().Foreground(muted).Render("  MCP & SKILL CONTROL")
	}
	space := max(1, w-lipgloss.Width(name)-lipgloss.Width(desc)-len(busy)-3)
	return widthLimit(name+desc+strings.Repeat(" ", space)+lipgloss.NewStyle().Foreground(color).Render(busy), w)
}

func (m Model) tabLine(w int) string {
	parts := make([]string, 0, sectionCount)
	for id, name := range sections {
		label := fmt.Sprintf(" %d %s ", id+1, name)
		style := lipgloss.NewStyle().Foreground(muted)
		if section(id) == m.section {
			style = style.Foreground(white).Background(navy).Bold(true)
		}
		parts = append(parts, style.Render(label))
	}
	return widthLimit(strings.Join(parts, " "), w)
}

func (m Model) sectionLabel(w int) string {
	count := m.itemCount()
	if m.section == diagnosticSection {
		count = 0
		if m.config != nil {
			count = len(m.config.Warnings)
		}
	}
	message := fmt.Sprintf("%s  ·  %d items", sections[m.section], count)
	if m.config == nil {
		message += "  ·  loading configuration"
	}
	return lipgloss.NewStyle().Foreground(white).Bold(true).Render(widthLimit(message, w))
}

func (m Model) content(w int) []string {
	rows := m.rows(w)
	details := m.details(w)
	if !m.showDetail || m.height < 21 {
		details = nil
	}
	// Reserve the header, selected item details and footer first. The list
	// scrolls independently so the highlighted item remains visible on resize.
	reserved := 3 + 1 + 2 + len(details) + 1
	if m.notice != "" {
		reserved++
	}
	maxRows := max(2, m.height-reserved)
	selected := m.selected[m.section]
	start := max(0, selected-maxRows+1)
	start = min(start, max(0, len(rows)-maxRows))
	if len(rows) == 0 {
		rows = []string{plain("No items discovered.", w)}
	}
	if start < len(rows) {
		rows = rows[start:min(len(rows), start+maxRows)]
	}
	if len(details) != 0 {
		rows = append(rows, lipgloss.NewStyle().Foreground(navy).Render(strings.Repeat("─", w)))
		rows = append(rows, details...)
	}
	return rows
}

func (m Model) rows(w int) []string {
	var lines []string
	switch m.section {
	case harnessSection:
		for idx, info := range harnesses {
			state := "[ ]"
			if !info.UserScopeEnabled {
				state = "[×]"
			} else if m.config != nil && enabledForHarness(m.config.Settings.Harnesses, info.ID) {
				state = "[✓]"
			}
			label := fmt.Sprintf("%s %-17s  %2d MCP   %2d skills", state, info.Label,
				m.availableServers(info.ID), m.availableSkills(info.ID))
			if !info.UserScopeEnabled {
				label += "  user scope unavailable"
			}
			lines = append(lines, rowText(label, w, idx == m.selected[m.section]))
		}
	case mcpSection:
		for idx, name := range m.serverNames() {
			server := m.config.Servers[name]
			state := "idle"
			if m.statusErr != nil {
				state = "unknown"
			} else if !m.status.Running {
				state = "not started"
			} else if _, live := m.activeBackend(name); live {
				state = "running"
			}
			label := fmt.Sprintf("%-27s %-10s %-9s %s", name, server.Type, sharing(server), state)
			lines = append(lines, rowText(label, w, idx == m.selected[m.section]))
		}
	case skillSection:
		if m.config != nil {
			for idx, skill := range m.config.Skills {
				count := 0
				for _, h := range harnesses {
					if h.UserScopeEnabled && enabledForHarness(m.config.Settings.Harnesses, h.ID) && config.Allows(skill.Harnesses, h.ID) {
						count++
					}
				}
				label := fmt.Sprintf("%-43s  %d active harness(es)", skill.Name, count)
				lines = append(lines, rowText(label, w, idx == m.selected[m.section]))
			}
		}
	case pluginSection:
		for idx, plugin := range m.plugins {
			state := "available"
			if !plugin.Valid {
				state = "warning"
			}
			label := fmt.Sprintf("%-28s %2d MCP  %2d skills  %s", plugin.Name, plugin.MCP, plugin.Skills, state)
			lines = append(lines, rowText(label, w, idx == m.selected[m.section]))
		}
	case diagnosticSection:
		if m.config != nil {
			for idx, warning := range m.config.Warnings {
				lines = append(lines, rowText(fmt.Sprintf("%2d  %s", idx+1, warning), w, idx == m.selected[m.section]))
			}
		}
		if len(lines) == 0 {
			lines = append(lines, tone("✓ No validation warnings.", green, w))
		}
	}
	return lines
}

func (m Model) details(w int) []string {
	if m.config == nil {
		return nil
	}
	var lines []string
	switch m.section {
	case harnessSection:
		info := harnesses[m.selected[m.section]]
		label := "disabled"
		if enabledForHarness(m.config.Settings.Harnesses, info.ID) {
			label = "enabled"
		}
		if !info.UserScopeEnabled {
			label = "unavailable for user-scope sync"
		}
		lines = []string{
			"Selected: " + info.Label + "  ·  " + label,
			"MCP config: " + info.ConfigFile,
			"Skills: " + info.SkillDirectory,
			"Space toggles harness availability; sync --apply updates native settings.",
		}
		if info.Note != "" {
			lines = append(lines, info.Note)
		}
	case mcpSection:
		names := m.serverNames()
		if len(names) == 0 {
			return []string{plain("Add MCP servers in ~/.agentmux/mcp.json or a plugin.", w)}
		}
		name := names[m.selected[m.section]]
		server := m.config.Servers[name]
		origin := "local command"
		if server.Type != "stdio" {
			endpoint, err := url.Parse(server.URL)
			if err == nil {
				origin = endpoint.Scheme + "://" + endpoint.Host
			} else {
				origin = "remote endpoint"
			}
		} else if server.Command != "" {
			origin = server.Command + " (arguments and environment hidden)"
		}
		lines = []string{
			"Source: " + server.Source,
			"Transport: " + server.Type + "  ·  " + origin,
			"Enabled harnesses: " + m.enabledHarnesses(server.Harnesses),
			"Sharing: " + sharing(server) + "   (press i to switch shared/isolated)",
		}
		if backend, live := m.activeBackend(name); live {
			lines = append(lines, fmt.Sprintf("%d active process(es)  ·  %d clients  ·  %d pending requests", m.backendCount(name), backend.Clients, backend.Pending))
		} else {
			lines = append(lines, "MCP process starts on demand; status does not probe upstream.")
		}
	case skillSection:
		if len(m.config.Skills) == 0 {
			return []string{plain("Place skills in ~/.agentmux/skills or a validated plugin.", w)}
		}
		skill := m.config.Skills[m.selected[m.section]]
		lines = []string{
			"Source: " + skill.Path,
			"Available to: " + m.enabledHarnesses(skill.Harnesses),
			"Press a or Enter to manage harness access in source SKILL.md.",
		}
	case pluginSection:
		if len(m.plugins) == 0 {
			return []string{plain("Place portable Agent Plugins in ~/.agentmux/plugins.", w)}
		}
		plugin := m.plugins[m.selected[m.section]]
		lines = []string{"Source: " + plugin.Path}
		if plugin.Description != "" {
			lines = append(lines, plugin.Description)
		}
		if plugin.Problem != "" {
			lines = append(lines, "Validation: "+plugin.Problem)
		} else {
			lines = append(lines, "Portable manifest loaded; MCP/skill availability follows harness filters.")
		}
	case diagnosticSection:
		brokerState := "stopped (no backend processes started)"
		if m.status.Running {
			brokerState = fmt.Sprintf("running (PID %d; %d backend processes)", m.status.PID, len(m.status.Backends))
		}
		lines = []string{
			"Broker: " + brokerState,
			fmt.Sprintf("Sources: %d MCP  ·  %d skills  ·  %d plugins", len(m.config.Servers), len(m.config.Skills), len(m.plugins)),
			"Runtime: idle " + m.config.Settings.Runtime.IdleTimeout + ", request " + m.config.Settings.Runtime.RequestTimeout,
		}
		if m.statusErr != nil {
			lines = append(lines, "IPC status: "+m.statusErr.Error())
		}
		lines = append(lines, "r refreshes source and broker diagnostics; no MCP processes are started.")
	}
	for i := range lines {
		lines[i] = lipgloss.NewStyle().Foreground(muted).Render(widthLimit(safeText("  "+lines[i]), w))
	}
	return lines
}

func (m Model) helpView(w int) string {
	entries := []string{
		"◆ AGENTMUX   ·   KEYBOARD REFERENCE", "",
		"1–5                    Select Harnesses / MCP / Skills / Plugins / Diagnostics",
		"Tab / Shift+Tab       Switch between sections",
		"← →  or  h l          Switch between sections",
		"↑ ↓  or  j k          Select an item",
		"Home / End  or  g G   First / last item",
		"Space / Enter          Toggle selected harness; show details for MCP/plugins",
		"a / Enter              Open a skill's per-harness availability dialog",
		"Dialog: ↑↓ navigate, Space toggle harness, Esc close",
		"s                      Preview native user-scope sync (read-only)",
		"Sync: ↑↓ scroll, y confirm and apply, r rebuild, Esc cancel",
		"i                      Switch selected MCP shared / isolated",
		"d                      Toggle detail panel",
		"r                      Refresh configuration and broker status",
		"? / Esc                Show / close this reference",
		"q / Ctrl+C             Quit", "",
		"Editing settings or SKILL.md creates a timestamped backup.",
		"Harness files are only changed after sync confirmation (s, then y).",
		"Sync preserves the exact contents of source SKILL.md in copies/links.",
		"VS Code user-scope sync is unavailable; project/profile setup is separate.",
	}
	result := make([]string, 0, m.height)
	for _, entry := range entries {
		result = append(result, plain(entry, w))
	}
	for len(result) < m.height {
		result = append(result, "")
	}
	return strings.Join(result[:min(len(result), m.height)], "\n")
}

func (m Model) skillModalView() string {
	width := max(38, m.width)
	innerWidth := max(25, min(68, width-10))
	options := m.skillModal.options
	selected := m.skillModal.selected
	rows := []string{
		lipgloss.NewStyle().Foreground(blue).Bold(true).Render("SKILL ACCESS"),
		lipgloss.NewStyle().Foreground(white).Bold(true).Render(widthLimit(safeText(m.skillModal.skill.Name), innerWidth)),
		lipgloss.NewStyle().Foreground(muted).Render("Which harnesses can use this skill?"),
		"",
	}
	maxList := max(1, m.height-11)
	start := max(0, selected-maxList+1)
	start = min(start, max(0, len(options)-maxList))
	for idx := start; idx < min(len(options), start+maxList); idx++ {
		option := options[idx]
		check := "[ ]"
		if config.Allows(m.skillModal.skill.Harnesses, option.ID) {
			check = "[✓]"
		}
		marker := "  "
		if idx == selected {
			marker = "› "
		}
		row := marker + check + " " + option.Label
		style := lipgloss.NewStyle().Foreground(white)
		if idx == selected {
			style = style.Background(navy).Bold(true)
		}
		rows = append(rows, style.Render(widthLimit(row, innerWidth)))
	}
	if len(options) > maxList {
		rows = append(rows, lipgloss.NewStyle().Foreground(muted).Render(fmt.Sprintf("%d–%d of %d harnesses", start+1, min(len(options), start+maxList), len(options))))
	}
	rows = append(rows, "")
	if m.busy || m.loading {
		rows = append(rows, lipgloss.NewStyle().Foreground(yellow).Render("Saving SKILL.md…"))
	} else {
		rows = append(rows, lipgloss.NewStyle().Foreground(muted).Render("↑↓ select  ·  Space toggle  ·  Esc close"))
	}
	if m.notice != "" {
		rows = append(rows, lipgloss.NewStyle().Foreground(yellow).Render(widthLimit(safeText(m.notice), innerWidth)))
	}
	panel := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(blue).
		Padding(0, 1).Width(innerWidth + 2).Render(strings.Join(rows, "\n"))
	return lipgloss.Place(width, m.height, lipgloss.Center, lipgloss.Center, panel)
}

func rowText(value string, width int, selected bool) string {
	value = "  " + widthLimit(safeText(value), max(1, width-2))
	if !selected {
		return lipgloss.NewStyle().Foreground(white).Render(value)
	}
	return lipgloss.NewStyle().Foreground(white).Background(navy).Bold(true).Width(width).Render(value)
}

func plain(value string, width int) string { return widthLimit(safeText(value), width) }

func tone(value string, color lipgloss.Color, width int) string {
	return lipgloss.NewStyle().Foreground(color).Render(widthLimit(safeText(value), width))
}

func widthLimit(value string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(value, width, "…")
}

// Source metadata is data, not terminal control input. Remove ANSI/control
// sequences before adding our own styling; otherwise a malformed manifest or
// warning could inject terminal cursor movement into the TUI.
func safeText(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}

// This function deliberately only displays the path. It does not write native
// config files, even when a resource is toggled in the source settings.
func settingsFile(root string) string {
	return filepath.Join(root, ".agentmux", "agentmux.json")
}
