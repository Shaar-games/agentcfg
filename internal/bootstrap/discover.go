package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentcfg/internal/pathutil"
	"agentcfg/internal/syncer"
)

type target struct {
	name    string
	configs []string
	skills  []string
	key     string
	toml    bool
}

// User-scope targets match internal/syncer/registry.go. VS Code is deliberately
// absent: that registry cannot resolve its platform/profile user configuration.
var targets = []target{
	{name: "claude-code", configs: []string{".claude.json"}, skills: []string{".claude/skills"}, key: "mcpServers"},
	{name: "codex", configs: []string{".codex/config.toml"}, skills: []string{".agents/skills", ".codex/skills"}, key: "mcp_servers", toml: true},
	{name: "cursor", configs: []string{".cursor/mcp.json"}, skills: []string{".cursor/skills"}, key: "mcpServers"},
	{name: "opencode", configs: syncer.OpenCodeUserConfigPaths(), skills: []string{".config/opencode/skills", ".opencode/skills"}, key: "mcp"},
	{name: "gemini", configs: []string{".gemini/settings.json"}, skills: []string{".gemini/skills"}, key: "mcpServers"},
	{name: "antigravity", configs: []string{".gemini/config/mcp_config.json"}, skills: []string{".gemini/config/skills"}, key: "mcpServers"},
}

// Discover examines only explicit, user-scope locations beneath home. It never
// infers a home from the current directory, environment or os.UserHomeDir.
func Discover(home string) (*Plan, error) {
	if strings.TrimSpace(home) == "" || !filepath.IsAbs(home) {
		return nil, errors.New("bootstrap discovery requires an explicit absolute home directory")
	}
	absolute, err := filepath.Abs(home)
	if err != nil {
		return nil, errors.New("invalid discovery home")
	}
	// Preserve the caller's spelling for reviewable source paths. On macOS
	// /var is a system alias of /private/var; replacing it here would make
	// scanner proofs and their displayed origins disagree with native paths.
	// anchoredPath compares the physically resolved root and candidate instead.
	if _, err = pathutil.Resolve(absolute); err != nil {
		return nil, errors.New("discovery home must exist")
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return nil, errors.New("discovery home must be a directory")
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, errors.New("cannot open discovery home")
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("discovery home changed while opening it")
	}
	if _, err := anchoredRootPath(root); err != nil {
		return nil, errors.New("discovery home changed while opening it")
	}
	plan := &Plan{Home: absolute, Harnesses: []Harness{}, Servers: []ServerCandidate{}, Skills: []SkillCandidate{}, Warnings: []Warning{}, sourceHashes: map[string][32]byte{}, homeIdentity: opened, budget: newScanBudget()}
	for _, target := range targets {
		harness := Harness{Name: target.name, Evidence: []string{}}
		var configs []string
		for _, relative := range target.configs {
			source := filepath.Join(absolute, filepath.FromSlash(relative))
			path, err := nativeSourcePath(source)
			if path == "" && err == nil {
				continue
			}
			if err != nil {
				plan.warn(target.name, source, "native config is unreadable or has an invalid link")
				continue
			}
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				plan.warn(target.name, source, "native config must be a regular file")
				continue
			}
			harness.Evidence = append(harness.Evidence, source)
			configs = append(configs, source)
		}
		for _, source := range configs {
			discoverServers(root, plan, target, source)
		}
		for _, relative := range target.skills {
			source := filepath.Join(absolute, filepath.FromSlash(relative))
			path, err := nativeSourcePath(source)
			if path == "" && err == nil {
				continue
			}
			if err != nil {
				plan.warnSkill(target.name, source, "native skills path is unreadable or has an invalid link")
				continue
			}
			info, err := os.Stat(path)
			if err != nil || !info.IsDir() {
				plan.warnSkill(target.name, source, "native skills path must be a directory")
				continue
			}
			harness.Evidence = append(harness.Evidence, source)
			discoverSkills(root, plan, target.name, source)
		}
		sort.Strings(harness.Evidence)
		harness.Detected = len(harness.Evidence) > 0
		harness.Selected = harness.Detected
		plan.Harnesses = append(plan.Harnesses, harness)
	}
	if _, err := anchoredRootPath(root); err != nil {
		return nil, errors.New("discovery home changed during discovery")
	}
	mergeCandidates(plan)
	return plan, nil
}

// Missing native locations are normal. A dangling link at the location or any
// ancestor is existing user configuration and must be reported before writes.
func nativeSourcePath(source string) (string, error) {
	path, err := pathutil.Resolve(source)
	if os.IsNotExist(err) {
		if _, potentialErr := pathutil.ResolvePotential(source); potentialErr == nil {
			return "", nil
		}
	}
	return path, err
}

func contained(root, path string) (string, error) {
	physicalRoot, err := pathutil.Resolve(root)
	if err != nil {
		return "", err
	}
	resolved, err := pathutil.Resolve(path)
	if err != nil {
		return "", err
	}
	// Presentation keeps the caller's root spelling. Actual access separately
	// verifies the opened root identity in anchoredPath before deriving a path.
	relative, err := relativeWithin(physicalRoot, resolved)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, relative), nil
}

func within(root, path string) bool {
	_, err := relativeWithin(root, path)
	return err == nil
}

func relativeWithin(root, path string) (string, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("path escapes discovery boundary")
	}
	return relative, nil
}

func (p *Plan) warn(harness, source, message string) {
	p.Warnings = append(p.Warnings, Warning{Harness: harness, Source: source, Message: message})
}

func (p *Plan) warnSkill(harness, source, message string) {
	p.Warnings = append(p.Warnings, Warning{Harness: harness, Source: source, Message: message, Kind: WarningSkill})
}
