package syncer

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentcfg/internal/config"
)

type Options struct {
	Root, Home, Binary, Scope, SkillMode string
	// AdoptExisting is reserved for explicitly confirmed init --apply imports.
	// Every existing entry also needs a scanner-captured proof below.
	AdoptExisting bool
	AdoptMCP      []MCPAdoption
	AdoptSkills   []SkillAdoption
}

type Change struct {
	Path         string `json:"path"`
	Action       string `json:"action"`
	before       []byte
	after        []byte
	exists       bool
	skill        *skillChange
	remove       bool
	source       string
	sourceHash   string
	sourceExists bool
	adopted      bool
}

type manifest struct {
	Version      int                          `json:"version"`
	Configs      map[string]map[string]string `json:"configs"`
	Skills       map[string]skillRecord       `json:"skills"`
	Blocks       map[string]string            `json:"tomlBlocks,omitempty"`
	Instructions map[string]string            `json:"instructions,omitempty"`
}

type Plan struct {
	Changes               []Change `json:"changes"`
	Warnings              []string `json:"warnings"`
	root, base, statePath string
	stateBefore           []byte
	stateExists           bool
	state                 manifest
	sourceConfigDigest    string
	adoption              *adoptionState
	rootIdentity          os.FileInfo
	baseIdentity          os.FileInfo
}

func freshManifest() manifest {
	return manifest{Version: 1, Configs: map[string]map[string]string{}, Skills: map[string]skillRecord{}, Blocks: map[string]string{}, Instructions: map[string]string{}}
}

func readOptional(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func digest(value any) string {
	data, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func rawDigest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func normalizeOptions(c *config.Config, opts Options) (Options, error) {
	if opts.AdoptExisting && opts.Home == "" {
		return opts, errors.New("adoption requires an explicitly supplied home")
	}
	if opts.Root == "" {
		opts.Root = c.Root
	}
	var err error
	if opts.Root, err = filepath.Abs(opts.Root); err != nil {
		return opts, err
	}
	if opts.Home == "" {
		opts.Home, err = os.UserHomeDir()
		if err != nil {
			return opts, err
		}
	}
	if opts.Home, err = filepath.Abs(opts.Home); err != nil {
		return opts, err
	}
	if opts.Binary == "" {
		opts.Binary, err = os.Executable()
		if err != nil {
			return opts, err
		}
	}
	if opts.Binary, err = filepath.Abs(opts.Binary); err != nil {
		return opts, err
	}
	if opts.Scope == "" {
		opts.Scope = "project"
	}
	if opts.Scope != "project" && opts.Scope != "user" {
		return opts, errors.New("scope must be project or user")
	}
	// Use one target-key spelling for adoption and subsequent ordinary sync.
	// Otherwise /var or NTFS aliases can make an existing ownership record
	// disappear from the lookup even though the physical destination is equal.
	if opts.Root, err = normalizeScopeRoot(opts.Root); err != nil {
		return opts, err
	}
	if opts.Scope == "user" {
		if opts.Home, err = normalizeScopeRoot(opts.Home); err != nil {
			return opts, err
		}
	}
	if opts.SkillMode == "" {
		opts.SkillMode = "auto"
	}
	if opts.SkillMode != "auto" && opts.SkillMode != "copy" && opts.SkillMode != "symlink" {
		return opts, errors.New("skill-mode must be auto, copy or symlink")
	}
	return opts, nil
}

// BuildPlan performs no filesystem writes. It refuses unowned name collisions
// unless explicit scanner proofs and adoption consent authorize those entries.
// Changes to previously managed content always remain conflicts.
func BuildPlan(c *config.Config, opts Options) (*Plan, error) {
	opts, err := normalizeOptions(c, opts)
	if err != nil {
		return nil, err
	}
	base := opts.Root
	if opts.Scope == "user" {
		base = opts.Home
	}
	p := &Plan{root: opts.Root, base: base, statePath: filepath.Join(opts.Root, ".agentcfg", "sync-"+opts.Scope+".json"),
		state: freshManifest(), Changes: []Change{}, Warnings: append([]string{}, c.Warnings...)}
	p.rootIdentity, err = os.Lstat(p.root)
	if err != nil {
		return nil, err
	}
	p.baseIdentity, err = os.Lstat(p.base)
	if err != nil {
		return nil, err
	}
	// Protect the plan against changes to canonical MCP/harness settings between
	// dry-run and apply. Test fixtures sometimes construct Config directly; in
	// that case we cannot claim a matching on-disk snapshot and skip this guard.
	if current, loadErr := config.Load(c.Root); loadErr == nil && digest(current) == digest(c) {
		p.sourceConfigDigest = digest(c)
	}
	if opts.AdoptExisting && p.sourceConfigDigest == "" {
		return nil, errors.New("adoption requires a fresh canonical configuration loaded from disk")
	}
	p.stateBefore, p.stateExists, err = readOptional(p.statePath)
	if err != nil {
		return nil, err
	}
	old := freshManifest()
	if p.stateExists {
		if err := json.Unmarshal(p.stateBefore, &old); err != nil {
			return nil, fmt.Errorf("invalid sync ownership manifest: %w", err)
		}
		if old.Version != 1 {
			return nil, fmt.Errorf("unsupported sync ownership manifest version %d", old.Version)
		}
	}
	old, err = normalizeManifestPaths(old, base)
	if err != nil {
		return nil, err
	}
	all := registry(base, opts.Scope)
	selected, err := selectTargets(all, c.Settings.Harnesses, opts.Scope)
	if err != nil {
		return nil, err
	}
	p.adoption, err = prepareAdoption(c, opts, all, selected, old)
	if err != nil {
		return nil, err
	}
	knownPaths := map[string]bool{}
	for _, t := range all {
		knownPaths[t.path] = true
		desired := map[string]map[string]any{}
		if selected[t.name] {
			for name, server := range c.Servers {
				if config.Allows(server.Harnesses, t.name) {
					desired[name] = proxyEntry(t, opts.Binary, c.Root, name)
				}
			}
		}
		if len(desired) != 0 || len(old.Configs[t.path]) != 0 {
			// An unselected, never-managed harness does not belong to this
			// operation's write set. Its native directory may be a symlink,
			// without making unrelated harnesses impossible to synchronize.
			if err := ensureNoSymlinkParents(base, t.path); err != nil {
				return nil, err
			}
			if err := p.planConfig(t, desired, old); err != nil {
				return nil, err
			}
		}
		if err := p.planSkills(t, c, selected[t.name], opts.SkillMode, old); err != nil {
			return nil, err
		}
	}
	for path := range old.Configs {
		if !knownPaths[path] {
			return nil, fmt.Errorf("ownership manifest contains an unexpected config path %s; no changes made", path)
		}
	}
	for path := range old.Skills {
		valid := false
		for _, t := range all {
			if filepath.Dir(path) == t.skills {
				valid = true
			}
		}
		if !valid {
			return nil, fmt.Errorf("ownership manifest contains an unexpected skill path %s", path)
		}
	}
	if err := p.planInstructions(c, opts, selected, old); err != nil {
		return nil, err
	}
	sort.Slice(p.Changes, func(i, j int) bool { return p.Changes[i].Path < p.Changes[j].Path })
	if err := p.checkScopeIdentity(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Plan) planConfig(t target, desired map[string]map[string]any, old manifest) error {
	read := readOptional
	if p.adoption != nil && len(p.adoption.mcp[t.path]) != 0 {
		read = p.readAdoptionFile
	}
	before, exists, err := read(t.path)
	if err != nil {
		return err
	}
	previous, adopted, err := p.adoptedMCP(t, before, old.Configs[t.path])
	if err != nil {
		return fmt.Errorf("%s: %w", t.path, err)
	}
	var after []byte
	var owned map[string]string
	if t.toml {
		var blockHash string
		after, owned, blockHash, err = editTOML(before, desired, old.Configs[t.path], old.Blocks[t.path])
		if blockHash != "" {
			p.state.Blocks[t.path] = blockHash
		}
	} else {
		after, owned, err = editJSON(before, t.key, desired, previous)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", t.path, err)
	}
	if len(owned) != 0 {
		p.state.Configs[t.path] = owned
	}
	if adopted || !bytes.Equal(before, after) {
		action := "update MCP configuration"
		if !exists {
			action = "create MCP configuration"
		} else if adopted {
			action = "adopt existing MCP configuration"
		}
		p.Changes = append(p.Changes, Change{Path: t.path, Action: action, before: before, after: after, exists: exists, adopted: adopted})
	}
	return nil
}

// Refuse reparse/symlink traversals in generated target paths, including the
// file itself. The source tree can be resolved independently by discovery.
func ensureNoSymlinkParents(base, path string) error {
	root, err := normalizeScopeRoot(base)
	if err != nil {
		return err
	}
	normalized, err := scopePath(root, path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, normalized)
	if err != nil {
		return err
	}
	current := root
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if redirected(info) {
			return fmt.Errorf("refusing to write through a symlink or junction: %s", current)
		}
	}
	return nil
}
