package tui

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
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
		choices := overviewNavigationOptions(cfg, len(discoverPlugins(root, cfg)))
		category, err := s.selectAction("agentcfg  ·  Inventory", "", choices...)
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
		case "refresh":
			continue
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
	rows := make([]navigationStatus, 0, len(harnesses))
	for _, h := range harnesses {
		state := "disabled"
		if !h.UserScopeEnabled {
			state = "adapter unavailable"
		} else if enabledForHarness(cfg.Settings.Harnesses, h.ID) {
			state = "enabled"
		}
		rows = append(rows, navigationStatus{title: h.Label, detail: state, id: h.ID})
	}
	choices := statusNavigationOptions(rows)
	choices = append(choices, huh.NewOption("Back", "back"))
	for {
		choice, err := s.selectAction("Harnesses", "User-level MCP and skill locations.", choices...)
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
	rows := make([]navigationStatus, 0, len(names))
	for _, name := range names {
		server := cfg.Servers[name]
		rows = append(rows, navigationStatus{title: safeHuhText(name), columns: []string{server.Type, sharing(server)}, id: navigationItemValue(name)})
	}
	choices := statusNavigationOptions(rows)
	choices = append(choices, huh.NewOption("Back", "back"))
	for {
		choice, err := s.selectAction("MCP servers", "Transport and sharing mode for each MCP server.", choices...)
		if isCancelled(err) || choice == "back" || choice == "" {
			return nil
		}
		if err != nil {
			return err
		}
		name, ok := navigationItemName(choice)
		if !ok {
			return fmt.Errorf("unexpected MCP inventory selection")
		}
		server := cfg.Servers[name]
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
		if err := s.note("MCP: "+safeHuhText(name), info); err != nil && !isCancelled(err) {
			return err
		}
	}
}

func (s interfaceSession) listSkills(cfg *config.Config) error {
	rows := make([]navigationStatus, 0, len(cfg.Skills))
	for _, skill := range cfg.Skills {
		active := 0
		for _, h := range harnesses {
			if h.UserScopeEnabled && enabledForHarness(cfg.Settings.Harnesses, h.ID) && config.Allows(skill.Harnesses, h.ID) {
				active++
			}
		}
		rows = append(rows, navigationStatus{title: safeHuhText(skill.Name), detail: strconv.Itoa(active), id: navigationItemValue(skill.Name)})
	}
	choices := statusNavigationOptions(rows)
	choices = append(choices, huh.NewOption("Back", "back"))
	for {
		choice, err := s.selectAction("Agent Skills", "Counts: enabled harnesses. Enter views paths and availability.", choices...)
		if isCancelled(err) || choice == "back" || choice == "" {
			return nil
		}
		if err != nil {
			return err
		}
		name, ok := navigationItemName(choice)
		if !ok {
			return fmt.Errorf("unexpected skill inventory selection")
		}
		for _, skill := range cfg.Skills {
			if skill.Name != name {
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
			if err := s.note("Skill: "+safeHuhText(name), info); err != nil && !isCancelled(err) {
				return err
			}
			break
		}
	}
}
