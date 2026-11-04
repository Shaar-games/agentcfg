package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"agentcfg/internal/bootstrap"
	"agentcfg/internal/config"
	"agentcfg/internal/tui"

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
	NativeServers     []string
}

type initOptions struct {
	Harnesses []string // explicit choice, or nil to use autodetected defaults
	Plain     bool
	Yes       bool   // explicit approval of canonical creation and native migration
	Binary    string // optional persistent executable supplied by an embedding caller
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
	interactive := terminalPresentation(input, output, opts.Plain) && !opts.Yes
	names := make([]string, 0, len(proposal.Servers))
	for name := range proposal.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	skills := append([]skillImport(nil), proposal.Skills...)
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })

	if interactive {
		chosen, err := terminalHarnessSelection(ctx, input, output, configured)
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
		configured, err = config.ValidateUserHarnesses(chosen)
		if err != nil {
			return err
		}
	}
	if !opts.Plain && (interactive || opts.Yes) {
		if err := checkInitCoverage(proposal.Inventory, configured); err != nil {
			return err
		}
	}
	// Import the complete portable inventory; harness selection controls only
	// distribution. Discovery and migration never execute imported commands.
	servers := make(map[string]map[string]any, len(names))
	for _, name := range names {
		servers[name] = proposal.Servers[name]
	}
	var preview strings.Builder
	fmt.Fprintf(&preview, "Detected harnesses: %s\n", asList(proposal.DetectedHarnesses))
	fmt.Fprintf(&preview, "Enabled harnesses: %s\n", asList(configured))
	fmt.Fprintf(&preview, "Importable MCP servers: %d (selected %d)\n", len(proposal.Servers), len(servers))
	fmt.Fprintf(&preview, "Importable native skills: %d (selected %d)\n", len(proposal.Skills), len(skills))
	fmt.Fprintln(&preview, "Canonical target: ~/.agentcfg/ (single source of truth)")
	fmt.Fprintln(&preview, "MCPs use the agentcfg broker; skills are moved here and copied to selected harnesses.")
	if opts.Plain {
		for _, name := range proposal.NativeServers {
			fmt.Fprintf(&preview, "Kept native: %s (Codex desktop session runtime)\n", name)
		}
		for _, name := range names {
			fmt.Fprintf(&preview, "MCP: %s (%s)\n", name, proposal.ServerSources[name])
		}
		for _, skill := range skills {
			fmt.Fprintf(&preview, "Skill: %s (%s)\n", skill.Name, skill.Source)
		}
	}
	if len(proposal.Warnings) > 0 {
		fmt.Fprintf(&preview, "Skipped/unsupported sources (%d):\n", len(proposal.Warnings))
		for _, warning := range proposal.Warnings {
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
		confirmed, err := terminalConfirm(ctx, input, output, "Initialize agentcfg?", initReview(configured, len(servers), len(skills), len(proposal.Warnings), len(proposal.NativeServers)))
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
	binary, err := initExecutable(opts.Binary, len(servers) > 0 && len(configured) > 0)
	if err != nil {
		return err
	}
	if err := writeInitialSourcesContext(ctx, home, settings, servers, skills); err != nil {
		return err
	}
	if err := applyInitSync(ctx, home, binary, proposal.Inventory, servers, skills, configured); err != nil {
		return recoverFailedInit(home, err)
	}
	_, err = fmt.Fprintf(output, "Initialized: %d MCP(s), %d skill(s).\n", len(servers), len(skills))
	return err
}

func asList(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	return strings.Join(values, ", ")
}

func initReview(harnesses []string, servers, skills, skipped int, native ...int) string {
	var summary strings.Builder
	fmt.Fprintf(&summary, "%-10s%s\n", "Harnesses", asList(tui.HarnessNames(harnesses)))
	fmt.Fprintf(&summary, "%-10s%d\n%-10s%d", "MCPs", servers, "Skills", skills)
	if len(native) > 0 && native[0] > 0 {
		fmt.Fprintf(&summary, "\n%-10s%d", "Native", native[0])
	}
	if skipped > 0 {
		fmt.Fprintf(&summary, "\n%-10s%d\nDetails: agentcfg init --plain", "Skipped", skipped)
	}
	return summary.String()
}
