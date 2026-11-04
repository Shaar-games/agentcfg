package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"agentcfg/internal/bootstrap"
	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"
	"agentcfg/internal/syncer"
)

func initExecutable(binary string, required bool) (string, error) {
	var err error
	if binary == "" {
		binary, err = os.Executable()
	}
	if err != nil {
		return "", err
	}
	if !required {
		return binary, nil
	}
	binary, err = pathutil.Resolve(binary)
	if err != nil {
		return "", fmt.Errorf("locate installed agentcfg executable: %w", err)
	}
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("native sync requires a regular installed agentcfg executable: %s", binary)
	}
	for _, part := range strings.Split(filepath.ToSlash(binary), "/") {
		if strings.HasPrefix(part, "go-build") {
			return "", errors.New("initialization requires a persistent agentcfg executable; build or install agentcfg first")
		}
	}
	return binary, nil
}

// A completed init must not leave active native sources outside the canonical
// inventory. Report unsupported/ambiguous inputs before publishing any source.
func checkInitCoverage(inventory *bootstrap.Plan, harnesses []string) error {
	if inventory == nil {
		return nil
	}
	for _, warning := range inventory.Warnings {
		if !warning.BlocksInit(harnesses) {
			continue
		}
		location := warning.Source
		if warning.Server != "" {
			location += fmt.Sprintf(" (MCP %q)", warning.Server)
		}
		return fmt.Errorf("initialization needs a complete import: %s: %s; inspect agentcfg init --plain", location, warning.Message)
	}
	for _, server := range inventory.Servers {
		if server.Selected {
			continue
		}
		for _, origin := range server.Origins {
			if slices.Contains(harnesses, origin.Harness) {
				return fmt.Errorf("resolve conflicting native MCP %q before initialization; inspect agentcfg init --plain", server.Name)
			}
		}
	}
	counts := map[string]int{}
	for _, skill := range inventory.Skills {
		counts[skill.Name]++
	}
	for name, count := range counts {
		if count > 1 {
			return fmt.Errorf("resolve conflicting native skill %q before initialization; inspect agentcfg init --plain", name)
		}
	}
	return nil
}

// Called only after the single init confirmation, or explicit --yes approval.
func applyInitSync(ctx context.Context, home, binary string, inventory *bootstrap.Plan,
	servers map[string]map[string]any, skills []skillImport, harnesses []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	mcp, nativeSkills, err := initAdoptionProofs(inventory, servers, skills, harnesses)
	if err != nil {
		return err
	}
	plan, err := syncer.BuildPlan(cfg, syncer.Options{
		Root: cfg.Root, Home: cfg.Root, Scope: "user", Binary: binary, SkillMode: "copy",
		AdoptExisting: true, AdoptMCP: mcp, AdoptSkills: nativeSkills,
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return plan.Apply()
}

// Failed initialization returns to the native baseline. Keep the imported
// sources in an archive so a failed attempt never destroys collected data.
func recoverFailedInit(home string, cause error) error {
	restore, err := syncer.BuildRestorePlan(home)
	if err == nil {
		err = restore.Apply()
	}
	if err != nil {
		return fmt.Errorf("initialization incomplete: %w; recovery failed: %v; sources and backups remain in ~/.agentcfg", cause, err)
	}
	return fmt.Errorf("initialization failed: %w; native settings restored, imported sources archived at %s", cause, restore.Archive)
}
