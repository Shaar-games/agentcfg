package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"agentcfg/internal/bootstrap"
	"agentcfg/internal/config"
)

// initialize discovers native user-scope harness MCPs and skills before any
// writes. Only the explicit --yes or successful Huh confirmation publishes a
// fresh canonical user configuration; --apply separately proposes native sync.
func initialize(ctx context.Context, flags *flag.FlagSet, root *string, args []string, input io.Reader, output io.Writer) error {
	harnesses := flags.String("harnesses", "", "comma-separated harness identifiers; default: autodetect native user configuration")
	plain := flags.Bool("plain", false, "preview discovery without creating configuration")
	apply := flags.Bool("apply", false, "after creating canonical sources, preview and confirm native user sync")
	yes := flags.Bool("yes", false, "explicitly approve initialization without interactive Huh prompts")
	if err := parse(flags, args); err != nil {
		return helpResult(err)
	}
	explicit := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "harnesses" {
			explicit = true
		}
	})
	opts := initOptions{Plain: *plain, Apply: *apply, Yes: *yes}
	if explicit {
		opts.Harnesses = []string{}
		if strings.TrimSpace(*harnesses) != "" {
			for _, item := range strings.Split(*harnesses, ",") {
				opts.Harnesses = append(opts.Harnesses, strings.TrimSpace(item))
			}
		}
		if _, err := config.ValidateUserHarnesses(opts.Harnesses); err != nil {
			return fmt.Errorf("init harnesses: %w", err)
		}
	}
	plan, err := bootstrap.Discover(*root)
	if err != nil {
		return fmt.Errorf("inspect native harness configuration: %w", err)
	}
	proposal, err := proposalFromNative(plan)
	if err != nil {
		return err
	}
	return runInitWizard(ctx, *root, proposal, opts, input, output)
}

// proposalFromNative converts only the scanner's vetted, selected candidates
// into portable canonical entries. Duplicate names are excluded (not renamed
// or overwritten), and server definitions retain discovered harness filters.
func proposalFromNative(plan *bootstrap.Plan) (initProposal, error) {
	if plan == nil {
		return initProposal{}, errors.New("missing native discovery inventory")
	}
	proposal := initProposal{
		Inventory:     plan,
		Servers:       map[string]map[string]any{},
		ServerSources: map[string]string{},
		Warnings:      make([]string, 0, len(plan.Warnings)),
	}
	for _, h := range plan.Harnesses {
		if h.Detected && h.Selected {
			proposal.DetectedHarnesses = append(proposal.DetectedHarnesses, h.Name)
		}
	}
	if _, err := config.ValidateUserHarnesses(proposal.DetectedHarnesses); err != nil {
		return initProposal{}, fmt.Errorf("invalid discovered harnesses: %w", err)
	}
	for _, warning := range plan.Warnings {
		proposal.Warnings = append(proposal.Warnings, fmt.Sprintf("%s: %s", warning.Harness, warning.Message))
	}
	for _, candidate := range plan.Servers {
		if !candidate.Selected {
			for _, warning := range candidate.Warnings {
				proposal.Warnings = append(proposal.Warnings, fmt.Sprintf("MCP %s: %s", candidate.Name, warning))
			}
			continue
		}
		if candidate.Name == "" {
			return initProposal{}, errors.New("selected native MCP has no name")
		}
		if _, exists := proposal.Servers[candidate.Name]; exists {
			return initProposal{}, fmt.Errorf("duplicate discovered MCP %q; resolve collision before initializing", candidate.Name)
		}
		serialized, err := json.Marshal(candidate.Server)
		if err != nil {
			return initProposal{}, fmt.Errorf("encode imported MCP %q: %w", candidate.Name, err)
		}
		var server map[string]any
		if err := json.Unmarshal(serialized, &server); err != nil {
			return initProposal{}, err
		}
		if len(candidate.Harnesses) > 0 {
			server["harnesses"] = append([]string(nil), candidate.Harnesses...)
		}
		proposal.Servers[candidate.Name] = server
		proposal.ServerSources[candidate.Name] = strings.Join(candidate.Harnesses, ", ")
	}
	for _, candidate := range plan.Skills {
		if !candidate.Selected {
			for _, warning := range candidate.Warnings {
				proposal.Warnings = append(proposal.Warnings, fmt.Sprintf("skill %s: %s", candidate.Name, warning))
			}
			continue
		}
		if candidate.Name == "" || candidate.Source == "" {
			return initProposal{}, errors.New("selected native skill has no name or source")
		}
		proposal.Skills = append(proposal.Skills, skillImport{
			Name: candidate.Name, Source: candidate.Source, TreeSHA256: candidate.TreeSHA256,
		})
	}
	return proposal, nil
}
