// Package tui provides the user-level interactive Huh configuration interface.
// It loads the canonical home sources; neither opening a menu nor reading
// diagnostics starts MCP processes or rewrites native harness configuration.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"unicode"

	"agentcfg/internal/broker"
	"agentcfg/internal/config"
	"agentcfg/internal/syncer"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
)

type harnessInfo struct {
	ID, Label, ConfigFile, SkillDirectory string
	UserScopeEnabled                      bool
	Note                                  string
}

var harnesses = [...]harnessInfo{
	{"claude-code", "Claude Code", "~/.claude.json", "~/.claude/skills", true, ""},
	{"codex", "Codex", "~/.codex/config.toml", "~/.agents/skills", true, ""},
	{"cursor", "Cursor", "~/.cursor/mcp.json", "~/.cursor/skills", true, ""},
	{"opencode", "OpenCode", "~/.config/opencode/opencode.json", "~/.config/opencode/skills", true, "Existing opencode.jsonc is used when applicable."},
	{"gemini", "Gemini CLI", "~/.gemini/settings.json", "~/.gemini/skills", true, ""},
	{"antigravity", "Antigravity", "~/.gemini/config/mcp_config.json", "~/.gemini/config/skills", true, ""},
	{"vscode", "VS Code (adapter unavailable)", "", "", false, "No supported VS Code native configuration adapter is installed."},
}

type interfaceSession struct {
	ctx  context.Context
	root string
	in   io.Reader
	out  io.Writer
}

func (s interfaceSession) form(fields ...huh.Field) error {
	form := huh.NewForm(huh.NewGroup(fields...)).
		WithInput(s.in).WithOutput(s.out).WithTheme(Theme())
	// Huh switches to a numbered accessible prompt when TERM=dumb. Windows
	// consoles can still render the native checkbox UI in that environment;
	// only override the fallback when both streams are real terminals.
	if os.Getenv("AGENTCFG_ACCESSIBLE") == "1" {
		form.WithAccessible(true)
	} else if runtime.GOOS == "windows" {
		in, inOK := s.in.(*os.File)
		out, outOK := s.out.(*os.File)
		if inOK && outOK && term.IsTerminal(in.Fd()) && term.IsTerminal(out.Fd()) {
			form.WithAccessible(false)
		}
	}
	return form.RunWithContext(s.ctx)
}

func (s interfaceSession) selectAction(title, description string, choices ...huh.Option[string]) (string, error) {
	var selected string
	err := s.form(huh.NewSelect[string]().
		Title(title).Description(description).Options(choices...).Height(12).Value(&selected))
	return selected, err
}

func (s interfaceSession) note(title, description string) error {
	return s.form(huh.NewNote().Title(title).Description(safeHuhText(description)).Height(17))
}

// Huh Notes render Markdown. Backslashes in a native Windows path would be
// consumed as escapes. Use forward slashes for presentation, and drop terminal
// control characters from untrusted plugin descriptions or MCP error strings.
func safeHuhText(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}

func (s interfaceSession) confirm(title, description string) (bool, error) {
	var yes bool
	err := s.form(huh.NewConfirm().Title(title).Description(description).
		Affirmative("Apply").Negative("Cancel").Value(&yes))
	return yes, err
}

func isCancelled(err error) bool {
	return errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled)
}

// Run opens the simple native Huh checkbox list directly. Do not place a
// single-choice menu in front of the multiselect: users should see the
// available harnesses and their current checked states immediately.
func Run(ctx context.Context, root string, input io.Reader, output io.Writer) error {
	return (interfaceSession{ctx: ctx, root: root, in: input, out: output}).configureHarnesses()
}

// RunMenu is the complete user-level configuration entrypoint for "agentcfg config".
// Multi-choice settings still use native Huh checkboxes inside their sections.
func RunMenu(ctx context.Context, root string, input io.Reader, output io.Writer) error {
	s := interfaceSession{ctx: ctx, root: root, in: input, out: output}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cfg, err := config.Load(root)
		if err != nil {
			if noteErr := s.note("Invalid canonical settings", err.Error()+"\nRepair ~/.agentcfg/agentcfg.json before continuing."); noteErr != nil && !isCancelled(noteErr) {
				return noteErr
			}
			return err
		}
		status, statusErr := broker.GetStatus(ctx, cfg.Root)
		state := "broker idle"
		if statusErr != nil {
			state = "broker status unavailable"
		} else if status.Running {
			state = fmt.Sprintf("broker PID %d", status.PID)
		}
		choice, err := s.selectAction("agentcfg  ·  Configuration", "User-level settings  ·  "+state+"  ·  ↑↓ select, / filter, Enter open, Ctrl+C exit",
			huh.NewOption("Harnesses  ·  enable / disable", "harnesses"),
			huh.NewOption(fmt.Sprintf("MCP servers  ·  %d configured", len(cfg.Servers)), "mcp"),
			huh.NewOption(fmt.Sprintf("Skills  ·  %d available", len(cfg.Skills)), "skills"),
			huh.NewOption(fmt.Sprintf("Agent Plugins  ·  %d folders", len(discoverPlugins(root, cfg))), "plugins"),
			huh.NewOption(fmt.Sprintf("Diagnostics and broker status  ·  %d warning(s)", len(cfg.Warnings)), "diagnostics"),
			huh.NewOption("Preview and apply user-level synchronization", "sync"),
			huh.NewOption("Refresh configuration and status", "refresh"),
			huh.NewOption("Exit", "exit"))
		if isCancelled(err) {
			return nil
		}
		if err != nil {
			return err
		}
		switch choice {
		case "harnesses":
			err = s.configureHarnesses()
		case "mcp":
			err = s.configureMCP()
		case "skills":
			err = s.configureSkills()
		case "plugins":
			err = s.showPlugins()
		case "diagnostics":
			err = s.showDiagnostics()
		case "sync":
			err = s.configureSync()
		case "exit":
			return nil
		}
		if isCancelled(err) {
			continue
		}
		if err != nil {
			if reportErr := s.note("Operation failed", err.Error()+"\nNo automatic retry or silent overwrite."); reportErr != nil && !isCancelled(reportErr) {
				return reportErr
			}
		}
	}
}

func (s interfaceSession) configureHarnesses() error {
	cfg, err := config.Load(s.root)
	if err != nil {
		return err
	}
	choices := make([]huh.Option[string], 0, len(harnesses))
	for _, h := range harnesses {
		if !h.UserScopeEnabled {
			continue
		}
		choices = append(choices, huh.NewOption(h.Label, h.ID).
			Selected(slices.Contains(cfg.Settings.Harnesses, h.ID)))
	}
	var selected []string
	err = s.form(huh.NewMultiSelect[string]().
		Title("Harness availability").
		Description("Space toggles, Enter saves, Ctrl+C cancels. Run sync to apply changes to native harness settings.").
		Options(choices...).
		Value(&selected))
	if isCancelled(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := setHarnessSelection(s.root, selected, cfg.Settings.Harnesses); err != nil {
		if errors.Is(err, errNoChange) {
			return nil
		}
		return err
	}
	return nil
}

func (s interfaceSession) configureMCP() error {
	for {
		cfg, err := config.Load(s.root)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(cfg.Servers))
		for name := range cfg.Servers {
			names = append(names, name)
		}
		sort.Strings(names)
		choices := make([]huh.Option[string], 0, len(names)+1)
		for _, name := range names {
			choices = append(choices, huh.NewOption(fmt.Sprintf("%s  ·  %s  ·  %s", safeHuhText(name), cfg.Servers[name].Type, sharing(cfg.Servers[name])), name))
		}
		choices = append(choices, huh.NewOption("Back to main menu", "back"))
		name, err := s.selectAction("MCP servers", "Shared reduces processes; isolated is required for private session state.", choices...)
		if err != nil {
			return err
		}
		if name == "back" {
			return nil
		}
		server := cfg.Servers[name]
		choice, err := s.selectAction(name, fmt.Sprintf("Transport: %s  ·  Sharing: %s", server.Type, sharing(server)),
			huh.NewOption("Toggle shared / isolated", "toggle"),
			huh.NewOption("Show transport and routing details", "details"),
			huh.NewOption("Back", "back"))
		if err != nil {
			return err
		}
		switch choice {
		case "toggle":
			current := sharing(server)
			next := "isolated"
			if current == "isolated" {
				next = "shared"
			}
			if err := setServerSharing(s.root, name, next, current); err != nil {
				return err
			}
			if err := s.note("MCP sharing updated", fmt.Sprintf("%s: %s → %s\nChange saved to canonical serverOverrides. Running connections keep their existing mode.", name, current, next)); err != nil {
				return err
			}
		case "details":
			var allowed []string
			for _, h := range harnesses {
				if h.UserScopeEnabled && slices.Contains(cfg.Settings.Harnesses, h.ID) && config.Allows(server.Harnesses, h.ID) {
					allowed = append(allowed, h.Label)
				}
			}
			endpoint := server.Command
			if server.Type != "stdio" {
				endpoint = server.URL
			}
			if len(endpoint) > 200 {
				endpoint = endpoint[:200] + "…"
			}
			if err := s.note("MCP details", fmt.Sprintf("Name: %s\nTransport: %s\nSharing: %s\nEndpoint/command: %s\nEnabled harnesses: %s\n\nThe broker starts only when a client connects.", name, server.Type, sharing(server), endpoint, strings.Join(allowed, ", "))); err != nil {
				return err
			}
		}
	}
}

func sharing(server config.Server) string {
	if server.Sharing == "isolated" {
		return "isolated"
	}
	return "shared"
}

func enabledForHarness(names []string, name string) bool { return slices.Contains(names, name) }

func (s interfaceSession) configureSkills() error {
	for {
		cfg, err := config.Load(s.root)
		if err != nil {
			return err
		}
		choices := make([]huh.Option[string], 0, len(cfg.Skills)+1)
		for _, skill := range cfg.Skills {
			count := 0
			for _, h := range harnesses {
				if h.UserScopeEnabled && slices.Contains(cfg.Settings.Harnesses, h.ID) && config.Allows(skill.Harnesses, h.ID) {
					count++
				}
			}
			choices = append(choices, huh.NewOption(fmt.Sprintf("%s  ·  %d harness(es)", safeHuhText(skill.Name), count), skill.Name))
		}
		choices = append(choices, huh.NewOption("Back to main menu", "back"))
		name, err := s.selectAction("Agent Skills", "Select a source skill to edit metadata.harness; installed copies are never rewritten.", choices...)
		if err != nil {
			return err
		}
		if name == "back" {
			return nil
		}
		if err := s.configureSkill(name); err != nil {
			return err
		}
	}
}

func (s interfaceSession) configureSkill(name string) error {
	cfg, err := config.Load(s.root)
	if err != nil {
		return err
	}
	var selected *config.Skill
	for i := range cfg.Skills {
		if cfg.Skills[i].Name == name {
			selected = &cfg.Skills[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("skill %q no longer exists; refresh", name)
	}
	choices := make([]huh.Option[string], 0, len(harnesses))
	var enabled []string
	for _, h := range harnesses {
		label := h.Label
		if !h.UserScopeEnabled {
			label = "VS Code (legacy filter only)"
		}
		active := config.Allows(selected.Harnesses, h.ID)
		choices = append(choices, huh.NewOption(label, h.ID).Selected(active))
	}
	err = s.form(huh.NewMultiSelect[string]().
		Title("Skill availability: " + safeHuhText(name)).
		Description("Space toggles, Enter saves, Ctrl+C cancels. No selection means metadata.harness: none.").
		Options(choices...).
		Value(&enabled))
	if isCancelled(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := setSkillHarnessSelection(s.root, *selected, enabled); err != nil {
		if errors.Is(err, errNoChange) {
			return nil
		}
		return err
	}
	return nil
}

func (s interfaceSession) showPlugins() error {
	cfg, err := config.Load(s.root)
	if err != nil {
		return err
	}
	items := discoverPlugins(s.root, cfg)
	choices := make([]huh.Option[string], 0, len(items)+1)
	for _, p := range items {
		state := "available"
		if !p.Valid {
			state = "warning"
		}
		choices = append(choices, huh.NewOption(fmt.Sprintf("%s  ·  %d MCP / %d skill(s)  ·  %s", safeHuhText(p.Name), p.MCP, p.Skills, state), p.Path))
	}
	choices = append(choices, huh.NewOption("Back", "back"))
	for {
		choice, err := s.selectAction("Portable Agent Plugins", "Plugin manifests remain portable; no changes are made to plugin files.", choices...)
		if err != nil {
			return err
		}
		if choice == "back" {
			return nil
		}
		for _, p := range items {
			if p.Path == choice {
				info := fmt.Sprintf("Path: %s\nDescription: %s\nMCP servers: %d\nSkills: %d\nStatus: %t\n%s", p.Path, p.Description, p.MCP, p.Skills, p.Valid, p.Problem)
				if err := s.note("Plugin "+p.Name, info); err != nil {
					return err
				}
			}
		}
	}
}

func (s interfaceSession) showDiagnostics() error {
	cfg, err := config.Load(s.root)
	if err != nil {
		return err
	}
	status, statusErr := broker.GetStatus(s.ctx, cfg.Root)
	var message strings.Builder
	fmt.Fprintf(&message, "Canonical root: %s\nMCP servers: %d  ·  Skills: %d  ·  Plugins: %d\n", filepath.Join(cfg.Root, ".agentcfg"), len(cfg.Servers), len(cfg.Skills), len(discoverPlugins(s.root, cfg)))
	if statusErr != nil {
		fmt.Fprintf(&message, "Broker status error: %v\n", statusErr)
	} else if !status.Running {
		message.WriteString("Broker stopped (status does not launch it).\n")
	} else {
		fmt.Fprintf(&message, "Broker PID: %d\n", status.PID)
		for _, p := range status.Backends {
			fmt.Fprintf(&message, "  %s: PID %d, %d clients, %d pending, isolated=%t\n", p.Name, p.PID, p.Clients, p.Pending, p.Isolated)
		}
	}
	if len(cfg.Warnings) == 0 {
		message.WriteString("No validation warnings.")
	}
	for _, warning := range cfg.Warnings {
		fmt.Fprintf(&message, "Warning: %s\n", warning)
	}
	return s.note("Diagnostics and broker status", message.String())
}

func (s interfaceSession) configureSync() error {
	cfg, err := config.Load(s.root)
	if err != nil {
		return err
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	binary, err := stableExecutable()
	if err != nil {
		return err
	}
	plan, err := syncer.BuildPlan(cfg, syncer.Options{
		Root: cfg.Root, Home: cfg.Root, Scope: "user", Binary: binary, SkillMode: "auto",
	})
	if err != nil {
		return err
	}
	var preview strings.Builder
	fmt.Fprintf(&preview, "%d planned change(s). Nothing written before confirmation.\n\n", len(plan.Changes))
	for _, change := range plan.Changes {
		path := change.Path
		if relative, err := filepath.Rel(cfg.Root, path); err == nil && !strings.HasPrefix(relative, "..") {
			path = filepath.Join("~", relative)
		}
		fmt.Fprintf(&preview, "• %s: %s\n", change.Action, path)
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintf(&preview, "Warning: %s\n", warning)
	}
	preview.WriteString("Source SKILL.md content stays unchanged during distribution.")
	if err := s.note("User-scope synchronization preview", preview.String()); err != nil {
		return err
	}
	if len(plan.Changes) == 0 {
		return nil
	}
	yes, err := s.confirm("Apply synchronized harness configuration?", "Recheck all sources and native files before writing; preserve unmanaged entries.")
	if err != nil {
		return err
	}
	if !yes {
		return nil
	}
	if err := plan.Apply(); err != nil {
		return err
	}
	return s.note("Sync completed", fmt.Sprintf("Applied %d change(s). All changes were limited to the user home; skills were symlinked or copied without rewriting their content.", len(plan.Changes)))
}

func stableExecutable() (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("agentcfg binary must exist as a regular installed file: %w", err)
	}
	temporary, err := filepath.Abs(os.TempDir())
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(temporary, binary); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if strings.HasPrefix(part, "go-build") {
				return "", fmt.Errorf("cannot install a temporary go run binary in harness settings")
			}
		}
	}
	return binary, nil
}
