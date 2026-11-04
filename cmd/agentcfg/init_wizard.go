package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"agentcfg/internal/bootstrap"
	"agentcfg/internal/config"
	"agentcfg/internal/syncer"

	"github.com/charmbracelet/huh"
)

// initProposal consists only of vetted, read-only native discoveries. The
// default flow produces a preview and Huh selections before any write.
type initProposal struct {
	Inventory         *bootstrap.Plan // immutable discovery snapshot for revalidation after prompts
	DetectedHarnesses []string
	Servers           map[string]map[string]any
	ServerSources     map[string]string
	Skills            []skillImport
	Warnings          []string
}

type initOptions struct {
	Harnesses []string // explicit choice, or nil to use autodetected defaults
	Plain     bool
	Yes       bool // explicit non-interactive approval of canonical creation only
}

func runInitWizard(ctx context.Context, home string, proposal initProposal, opts initOptions, input io.Reader, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	configured := append([]string(nil), proposal.DetectedHarnesses...)
	if opts.Harnesses != nil {
		configured = append([]string(nil), opts.Harnesses...)
	}
	configured, err := config.ValidateUserHarnesses(configured)
	if err != nil {
		return err
	}
	available := []struct{ ID, Label string }{
		{"claude-code", "Claude Code"}, {"codex", "Codex"}, {"cursor", "Cursor"},
		{"opencode", "OpenCode"}, {"gemini", "Gemini CLI"}, {"antigravity", "Antigravity"},
	}
	interactive := terminalPresentation(input, output, opts.Plain) && !opts.Yes
	names := make([]string, 0, len(proposal.Servers))
	for name := range proposal.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	skills := append([]skillImport(nil), proposal.Skills...)
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })

	if interactive {
		choices := make([]huh.Option[string], 0, len(available))
		for _, h := range available {
			choices = append(choices, huh.NewOption(h.Label, h.ID).Selected(slices.Contains(configured, h.ID)))
		}
		var chosen []string
		if err := terminalForm(ctx, input, output, huh.NewMultiSelect[string]().
			Title("Initialize agentcfg: AI harnesses").
			Description("Detected harnesses are prechecked. Space toggles; Enter confirms the selection.").
			Options(choices...).Value(&chosen)); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
		configured, err = config.ValidateUserHarnesses(chosen)
		if err != nil {
			return err
		}
		names, err = selectImportedMCPs(ctx, input, output, names, proposal.ServerSources)
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
		skills, err = selectImportedSkills(ctx, input, output, skills)
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
	}
	// Generate only names selected in Huh. Never execute any imported command
	// and never import native secrets by treating config files as credentials.
	servers := make(map[string]map[string]any, len(names))
	for _, name := range names {
		servers[name] = proposal.Servers[name]
	}
	var preview strings.Builder
	fmt.Fprintf(&preview, "Detected harnesses: %s\n", asList(proposal.DetectedHarnesses))
	fmt.Fprintf(&preview, "Enabled harnesses: %s\n", asList(configured))
	fmt.Fprintf(&preview, "Importable MCP servers: %d (selected %d)\n", len(proposal.Servers), len(servers))
	fmt.Fprintf(&preview, "Importable native skills: %d (selected %d)\n", len(proposal.Skills), len(skills))
	fmt.Fprintln(&preview, "Canonical target: ~/.agentcfg/ (new user-level sources only)")
	fmt.Fprintln(&preview, "After creation, review native synchronization and confirm once before applying.")
	if len(proposal.Warnings) > 0 {
		fmt.Fprintf(&preview, "Skipped/unsupported sources (%d):\n", len(proposal.Warnings))
		for index, warning := range proposal.Warnings {
			if index >= 12 {
				fmt.Fprintf(&preview, "... and %d more warnings\n", len(proposal.Warnings)-index)
				break
			}
			fmt.Fprintf(&preview, "- %s\n", warning)
		}
	}
	if !interactive {
		if _, err := io.WriteString(output, preview.String()); err != nil {
			return err
		}
		if opts.Plain || !opts.Yes {
			_, err := fmt.Fprintln(output, "Preview only; nothing written. Use an interactive terminal to confirm or --yes to initialize explicitly.")
			return err
		}
	} else {
		if err := terminalNote(ctx, input, output, "Initialize: review proposed user-level sources", preview.String()); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
		confirmed, err := terminalConfirm(ctx, input, output, "Create canonical agentcfg configuration?", "New source files and copied skills only. Existing canonical or native files are not overwritten.")
		if err != nil {
			return err
		}
		if !confirmed {
			_, err := fmt.Fprintln(output, "Initialization cancelled; nothing written.")
			return err
		}
	}
	settings := config.Settings{
		Harnesses: configured,
		Runtime: config.Runtime{
			IdleTimeout: "5m", RequestTimeout: "5m",
		},
	}
	if proposal.Inventory != nil {
		// A Huh session can be open while other tools edit the native sources.
		// Re-discover before copying or writing any canonical file; a changed
		// source must be reviewed again instead of importing unseen content.
		if err := proposal.Inventory.Revalidate(); err != nil {
			return fmt.Errorf("native source changed after initialization preview: %w", err)
		}
		current, err := bootstrap.Discover(home)
		if err != nil {
			return fmt.Errorf("recheck native discovery before initialization: %w", err)
		}
		if !proposal.Inventory.SameInventory(current) {
			return errors.New("native harness configuration changed after the init preview; rerun agentcfg init before applying")
		}
	}
	if err := writeInitialSourcesContext(ctx, home, settings, servers, skills); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Created canonical agentcfg configuration for %d harness(es), %d MCP(s) and %d skill(s).\n",
		len(configured), len(servers), len(skills)); err != nil {
		return fmt.Errorf("canonical configuration created, but status output failed: %w", err)
	}
	// --yes is limited to canonical creation: a non-interactive confirmation
	// must not become implicit permission to replace native MCP or skill data.
	// The interactive flow offers one separate, final approval after preview.
	if !interactive {
		_, err := fmt.Fprintln(output, "Native harness configuration was not modified. Run agentcfg sync to preview later changes.")
		return err
	}
	return previewAndApplyInitSync(ctx, home, input, output, interactive, proposal.Inventory, servers, skills, configured)
}

func selectImportedMCPs(ctx context.Context, input io.Reader, output io.Writer, names []string, sources map[string]string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	choices := make([]huh.Option[string], 0, len(names))
	for _, name := range names {
		label := name
		if origin := sources[name]; origin != "" {
			label += "  ·  " + origin
		}
		choices = append(choices, huh.NewOption(label, name).Selected(true))
	}
	var selected []string
	err := terminalForm(ctx, input, output, huh.NewMultiSelect[string]().
		Title("Import existing MCP servers").
		Description("Space toggles an imported definition, Enter continues. Commands are never executed.").
		Options(choices...).Value(&selected))
	return selected, err
}

func selectImportedSkills(ctx context.Context, input io.Reader, output io.Writer, skills []skillImport) ([]skillImport, error) {
	if len(skills) == 0 {
		return nil, nil
	}
	var selected []string
	choices := make([]huh.Option[string], 0, len(skills))
	for _, skill := range skills {
		choices = append(choices, huh.NewOption(skill.Name, skill.Name).Selected(true))
	}
	err := terminalForm(ctx, input, output, huh.NewMultiSelect[string]().
		Title("Import existing native skills").
		Description("Copy source trees without changing SKILL.md content; Space toggles, Enter continues.").
		Options(choices...).Value(&selected))
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(selected))
	for _, name := range selected {
		allowed[name] = true
	}
	filtered := make([]skillImport, 0, len(selected))
	for _, skill := range skills {
		if allowed[skill.Name] {
			filtered = append(filtered, skill)
		}
	}
	return filtered, nil
}

func previewAndApplyInitSync(ctx context.Context, home string, input io.Reader, output io.Writer, interactive bool,
	inventory *bootstrap.Plan, selectedServers map[string]map[string]any, selectedSkills []skillImport, configured []string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !interactive {
		return errors.New("initialization cannot synchronize native harnesses without interactive confirmation")
	}
	cfg, err := config.Load(home)
	if err != nil {
		return fmt.Errorf("canonical configuration created; could not load for native sync: %w", err)
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return fmt.Errorf("locate installed agentcfg executable: %w", err)
	}
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("native sync requires a regular installed agentcfg executable: %s: %v", binary, err)
	}
	// go run uses temporary go-build executables. Installing one of those paths
	// into native harness configuration would break the next MCP invocation.
	for _, part := range strings.Split(filepath.ToSlash(binary), "/") {
		if strings.HasPrefix(part, "go-build") {
			return errors.New("native synchronization requires a persistent agentcfg executable, not a temporary go run build")
		}
	}
	adoptMCP, adoptSkills, err := initAdoptionProofs(inventory, selectedServers, selectedSkills, configured)
	if err != nil {
		return fmt.Errorf("canonical configuration created, but native adoption cannot be proved: %w", err)
	}
	plan, err := syncer.BuildPlan(cfg, syncer.Options{
		Root: cfg.Root, Home: cfg.Root, Scope: "user", Binary: binary, SkillMode: "auto",
		AdoptExisting: true, AdoptMCP: adoptMCP, AdoptSkills: adoptSkills,
	})
	if err != nil {
		return fmt.Errorf("canonical configuration created, native synchronization not applied: %w", err)
	}
	var preview strings.Builder
	fmt.Fprintf(&preview, "%d native change(s) proposed:\n", len(plan.Changes))
	for _, change := range plan.Changes {
		path := change.Path
		if rel, err := filepath.Rel(home, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			path = filepath.Join("~", rel)
		}
		fmt.Fprintf(&preview, "- %s: %s\n", change.Action, path)
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintf(&preview, "Warning: %s\n", warning)
	}
	fmt.Fprintf(&preview, "Explicit adoption: %d native MCP definition(s), %d native skill tree(s).\n", len(adoptMCP), len(adoptSkills))
	if len(plan.Changes) == 0 {
		_, err := fmt.Fprintln(output, "No native harness changes required.")
		return err
	}
	if err := terminalNote(ctx, input, output, "Native synchronization preview", preview.String()); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			_, writeErr := fmt.Fprintln(output, "Native synchronization cancelled; canonical sources were created.")
			return writeErr
		}
		return err
	}
	confirmed, err := terminalConfirm(ctx, input, output, "Synchronize native harnesses?", "Only the selected entries will be changed. Native files are rechecked and backed up. Choose No. to keep canonical sources without syncing.")
	if err != nil {
		return err
	}
	if !confirmed {
		_, err := fmt.Fprintln(output, "Native synchronization cancelled; canonical sources were created.")
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := plan.Apply(); err != nil {
		return fmt.Errorf("canonical created but native sync failed: %w", err)
	}
	_, err = fmt.Fprintf(output, "Applied %d native harness change(s).\n", len(plan.Changes))
	return err
}

func asList(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	return strings.Join(values, ", ")
}
