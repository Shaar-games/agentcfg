package tui

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"agentcfg/internal/config"

	"github.com/charmbracelet/huh"
)

// RunList browses the canonical inventory without modifying settings, skill
// sources, native harness files, or running MCP processes. A single-choice
// Select is used for every category and item navigation screen.
func RunList(ctx context.Context, root string, input io.Reader, output io.Writer) error {
	s := interfaceSession{ctx: ctx, root: root, in: input, out: output}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cfg, err := config.Load(root)
		if err != nil {
			return err
		}
		category, err := s.selectAction("agentcfg  ·  Inventory", "↑↓ navigate, Enter opens, Ctrl+C exits",
			huh.NewOption(fmt.Sprintf("Harnesses  ·  %d active", len(cfg.Settings.Harnesses)), "harnesses"),
			huh.NewOption(fmt.Sprintf("MCP servers  ·  %d configured", len(cfg.Servers)), "mcp"),
			huh.NewOption(fmt.Sprintf("Skills  ·  %d available", len(cfg.Skills)), "skills"),
			huh.NewOption(fmt.Sprintf("Agent Plugins  ·  %d folders", len(discoverPlugins(root, cfg))), "plugins"),
			huh.NewOption("Exit", "exit"))
		if isCancelled(err) {
			return nil
		}
		if err != nil {
			return err
		}
		switch category {
		case "harnesses":
			err = s.listHarnesses(cfg)
		case "mcp":
			err = s.listMCP(cfg)
		case "skills":
			err = s.listSkills(cfg)
		case "plugins":
			err = s.showPlugins()
		case "exit", "":
			return nil
		}
		if isCancelled(err) {
			continue
		}
		if err != nil {
			return err
		}
	}
}

func (s interfaceSession) listHarnesses(cfg *config.Config) error {
	choices := make([]huh.Option[string], 0, len(harnesses)+1)
	for _, h := range harnesses {
		state := "disabled"
		if !h.UserScopeEnabled {
			state = "adapter unavailable"
		} else if enabledForHarness(cfg.Settings.Harnesses, h.ID) {
			state = "enabled"
		}
		choices = append(choices, huh.NewOption(fmt.Sprintf("%s  ·  %s", h.Label, state), h.ID))
	}
	choices = append(choices, huh.NewOption("Back to inventory", "back"))
	for {
		choice, err := s.selectAction("Harnesses", "↑↓ navigate, Enter views native paths", choices...)
		if isCancelled(err) || choice == "back" || choice == "" {
			return nil
		}
		if err != nil {
			return err
		}
		for _, h := range harnesses {
			if h.ID != choice {
				continue
			}
			state := "disabled"
			if !h.UserScopeEnabled {
				state = "adapter unavailable"
			} else if enabledForHarness(cfg.Settings.Harnesses, h.ID) {
				state = "enabled"
			}
			info := fmt.Sprintf("Status: %s\nMCP config: %s\nSkills: %s", state, h.ConfigFile, h.SkillDirectory)
			if h.Note != "" {
				info += "\n" + h.Note
			}
			if err := s.note("Harness: "+h.Label, info); err != nil && !isCancelled(err) {
				return err
			}
			break
		}
	}
}

func (s interfaceSession) listMCP(cfg *config.Config) error {
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	choices := make([]huh.Option[string], 0, len(names)+1)
	for _, name := range names {
		server := cfg.Servers[name]
		choices = append(choices, huh.NewOption(fmt.Sprintf("%s  ·  %s  ·  %s", safeHuhText(name), server.Type, sharing(server)), name))
	}
	choices = append(choices, huh.NewOption("Back to inventory", "back"))
	for {
		choice, err := s.selectAction("MCP servers", "↑↓ navigate, Enter views details", choices...)
		if isCancelled(err) || choice == "back" || choice == "" {
			return nil
		}
		if err != nil {
			return err
		}
		server := cfg.Servers[choice]
		var enabled []string
		for _, h := range harnesses {
			if h.UserScopeEnabled && enabledForHarness(cfg.Settings.Harnesses, h.ID) && config.Allows(server.Harnesses, h.ID) {
				enabled = append(enabled, h.Label)
			}
		}
		info := fmt.Sprintf("Transport: %s\nSharing: %s\nEnabled harnesses: %s\nSource: %s", server.Type, sharing(server), strings.Join(enabled, ", "), server.Source)
		if server.Type == "stdio" {
			info += "\nCommand: " + server.Command
		} else {
			info += "\nHTTP endpoint: " + server.URL
		}
		if err := s.note("MCP: "+safeHuhText(choice), info); err != nil && !isCancelled(err) {
			return err
		}
	}
}

func (s interfaceSession) listSkills(cfg *config.Config) error {
	choices := make([]huh.Option[string], 0, len(cfg.Skills)+1)
	for _, skill := range cfg.Skills {
		active := 0
		for _, h := range harnesses {
			if h.UserScopeEnabled && enabledForHarness(cfg.Settings.Harnesses, h.ID) && config.Allows(skill.Harnesses, h.ID) {
				active++
			}
		}
		choices = append(choices, huh.NewOption(fmt.Sprintf("%s  ·  %d harness(es)", safeHuhText(skill.Name), active), skill.Name))
	}
	choices = append(choices, huh.NewOption("Back to inventory", "back"))
	for {
		choice, err := s.selectAction("Agent Skills", "↑↓ navigate, Enter views skill paths and availability", choices...)
		if isCancelled(err) || choice == "back" || choice == "" {
			return nil
		}
		if err != nil {
			return err
		}
		for _, skill := range cfg.Skills {
			if skill.Name != choice {
				continue
			}
			var enabled []string
			for _, h := range harnesses {
				if h.UserScopeEnabled && enabledForHarness(cfg.Settings.Harnesses, h.ID) && config.Allows(skill.Harnesses, h.ID) {
					enabled = append(enabled, h.Label)
				}
			}
			status := strings.Join(enabled, ", ")
			if status == "" {
				status = "none"
			}
			info := fmt.Sprintf("Source: %s\nAvailable to: %s\n\nListed only; SKILL.md content is not modified.", skill.Path, status)
			if err := s.note("Skill: "+safeHuhText(choice), info); err != nil && !isCancelled(err) {
				return err
			}
			break
		}
	}
}
