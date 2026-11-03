package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentcfg/internal/pathutil"
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
	{name: "codex", configs: []string{".codex/config.toml"}, skills: []string{".agents/skills"}, key: "mcp_servers", toml: true},
	{name: "cursor", configs: []string{".cursor/mcp.json"}, skills: []string{".cursor/skills"}, key: "mcpServers"},
	{name: "opencode", configs: []string{".config/opencode/opencode.json", ".config/opencode/opencode.jsonc", ".opencode/opencode.json", ".opencode/opencode.jsonc"}, skills: []string{".config/opencode/skills", ".opencode/skills"}, key: "mcp"},
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
	plan := &Plan{Home: absolute, Harnesses: []Harness{}, Servers: []ServerCandidate{}, Skills: []SkillCandidate{}, Warnings: []Warning{}, sourceHashes: map[string][32]byte{}, budget: newScanBudget()}
	for _, target := range targets {
		harness := Harness{Name: target.name, Evidence: []string{}}
		var configs []string
		for _, relative := range target.configs {
			source := filepath.Join(absolute, filepath.FromSlash(relative))
			path, err := anchoredPath(root, source)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				plan.warn(target.name, source, "native config is unreadable or escapes the discovery home")
				continue
			}
			info, err := root.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				plan.warn(target.name, source, "native config must be a regular file")
				continue
			}
			harness.Evidence = append(harness.Evidence, source)
			configs = append(configs, source)
		}
		for _, source := range configs {
			start := len(plan.Servers)
			discoverServers(root, plan, target, source)
			if len(configs) > 1 {
				for i := start; i < len(plan.Servers); i++ {
					plan.Servers[i].Selected = false
					plan.Servers[i].Warnings = append(plan.Servers[i].Warnings, "multiple native config locations exist; confirm which definition is active")
				}
			}
		}
		for _, relative := range target.skills {
			source := filepath.Join(absolute, filepath.FromSlash(relative))
			path, err := anchoredPath(root, source)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				plan.warn(target.name, source, "native skills path is unreadable or escapes the discovery home")
				continue
			}
			info, err := root.Stat(path)
			if err != nil || !info.IsDir() {
				plan.warn(target.name, source, "native skills path must be a directory")
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
	mergeCandidates(plan)
	return plan, nil
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
	if !within(physicalRoot, resolved) {
		return "", errors.New("path escapes discovery boundary")
	}
	// Reexpress the verified physical target under the caller's root spelling.
	// This preserves macOS /var aliases and Windows native source paths shown
	// in scanner proofs without permitting an actual escape from that root.
	relative, err := filepath.Rel(physicalRoot, resolved)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, relative), nil
}

func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func (p *Plan) warn(harness, source, message string) {
	p.Warnings = append(p.Warnings, Warning{Harness: harness, Source: source, Message: message})
}
