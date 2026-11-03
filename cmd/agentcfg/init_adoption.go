package main

import (
	"errors"
	"fmt"
	"slices"

	"agentcfg/internal/bootstrap"
	"agentcfg/internal/syncer"
)

// initAdoptionProofs translates only the user's explicitly selected native
// imports into precise scanner-proven native destinations. Origins preserve
// the harness/path pairing: Sources and Harnesses are independent unions.
// These proofs are NOT authority by themselves; the sync plan verifies every
// path and hash again and Apply requires a separate user confirmation.
func initAdoptionProofs(inventory *bootstrap.Plan, selectedServers map[string]map[string]any, selectedSkills []skillImport, harnesses []string) ([]syncer.MCPAdoption, []syncer.SkillAdoption, error) {
	if inventory == nil {
		return nil, nil, nil
	}
	if err := inventory.Revalidate(); err != nil {
		return nil, nil, fmt.Errorf("native discovery changed before adoption preview: %w", err)
	}
	var mcp []syncer.MCPAdoption
	var skills []syncer.SkillAdoption
	seenMCP := map[string]bool{}
	seenSkills := map[string]bool{}
	for _, candidate := range inventory.Servers {
		if !candidate.Selected {
			continue
		}
		if _, chosen := selectedServers[candidate.Name]; !chosen {
			continue
		}
		for _, origin := range candidate.Origins {
			if !slices.Contains(harnesses, origin.Harness) {
				continue
			}
			hash, ok := inventory.SourceSHA256(origin.Source)
			if !ok || hash == "" || candidate.CanonicalSHA256 == "" {
				return nil, nil, fmt.Errorf("native MCP %q has no verified scanner proof", candidate.Name)
			}
			key := origin.Harness + "\x00" + origin.Source + "\x00" + candidate.Name
			if seenMCP[key] {
				return nil, nil, errors.New("duplicate native MCP adoption origin")
			}
			seenMCP[key] = true
			mcp = append(mcp, syncer.MCPAdoption{
				Harness: origin.Harness, Path: origin.Source,
				Name: candidate.Name, CanonicalName: candidate.Name,
				FileSHA256: hash, CanonicalSHA256: candidate.CanonicalSHA256,
			})
		}
	}
	for _, candidate := range inventory.Skills {
		if !candidate.Selected {
			continue
		}
		chosen := false
		for _, skill := range selectedSkills {
			if skill.Name == candidate.Name && skill.Source == candidate.Source {
				chosen = true
				break
			}
		}
		if !chosen {
			continue
		}
		for _, origin := range candidate.Origins {
			if !slices.Contains(harnesses, origin.Harness) {
				continue
			}
			if candidate.TreeSHA256 == "" {
				return nil, nil, fmt.Errorf("native skill %q has no verified scanner tree fingerprint", candidate.Name)
			}
			key := origin.Harness + "\x00" + origin.Source
			if seenSkills[key] {
				return nil, nil, errors.New("duplicate native skill adoption origin")
			}
			seenSkills[key] = true
			skills = append(skills, syncer.SkillAdoption{
				Harness: origin.Harness, Path: origin.Source,
				CanonicalName: candidate.Name, TreeSHA256: candidate.TreeSHA256,
			})
		}
	}
	return mcp, skills, nil
}
