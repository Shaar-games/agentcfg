package syncer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const initRestoreFile = "init-restore.json"

type initRestorePoint struct {
	Version int            `json:"version"`
	Native  []restoreEntry `json:"native"`
}

type restoreEntry struct {
	Path         string `json:"path"` // relative to the user home
	Skill        bool   `json:"skill,omitempty"`
	BeforeExists bool   `json:"beforeExists"`
	Before       []byte `json:"before,omitempty"`
	BeforeHash   string `json:"beforeHash,omitempty"`
	Backup       string `json:"backup,omitempty"` // relative to .agentcfg
	AfterHash    string `json:"afterHash,omitempty"`
	AfterAbsent  bool   `json:"afterAbsent,omitempty"`
	AfterSource  string `json:"afterSource,omitempty"`
}

// CreateInitRestorePoint is called in the private canonical staging directory.
// Publishing init therefore publishes its recovery marker atomically as well.
func CreateInitRestorePoint(home string) error {
	path := filepath.Join(home, ".agentcfg", initRestoreFile)
	data, err := json.MarshalIndent(initRestorePoint{Version: 1, Native: []restoreEntry{}}, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	return errors.Join(writeErr, file.Close())
}

// captureInitRestore runs under the sync lock, before the first native write.
// Later syncs never replace the pre-init baseline. Existing installations with
// no marker are left untouched; guessing their original state would lose data.
func (p *Plan) captureInitRestore() error {
	if p.stateExists || p.root != p.base || len(p.Changes) == 0 {
		return nil
	}
	path := filepath.Join(p.root, ".agentcfg", initRestoreFile)
	if err := ensureNoSymlinkParents(p.root, path); err != nil {
		return err
	}
	data, exists, err := readOptional(path)
	if err != nil || !exists {
		return err
	}
	var point initRestorePoint
	if err := json.Unmarshal(data, &point); err != nil || point.Version != 1 {
		return errors.New("invalid initialization restore point")
	}
	if len(point.Native) != 0 {
		return nil
	}
	for _, change := range p.Changes {
		relative, err := filepath.Rel(p.base, change.Path)
		if err != nil || !withinScope(relative) {
			return errors.New("restore target escapes user home")
		}
		entry := restoreEntry{Path: filepath.ToSlash(relative), BeforeExists: change.exists, AfterAbsent: change.remove}
		if change.skill == nil {
			entry.Before = change.before
			entry.BeforeHash = rawDigest(change.before)
			entry.AfterHash = rawDigest(change.after)
		} else {
			entry.Skill = true
			entry.AfterAbsent = change.skill.remove
			entry.AfterSource = change.skill.source
			if !entry.AfterAbsent {
				entry.AfterHash, err = SkillTreeSHA256(change.skill.source)
				if err != nil {
					return err
				}
			}
			if change.exists {
				entry.Backup = filepath.ToSlash(filepath.Join("restore", "skills", rawDigest([]byte(relative))[:16]))
				backup := filepath.Join(p.root, ".agentcfg", filepath.FromSlash(entry.Backup))
				if err := ensureNoSymlinkParents(p.root, backup); err != nil {
					return err
				}
				entry.BeforeHash, err = SkillTreeSHA256(change.Path)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
					return err
				}
				if err := copySkill(change.Path, backup); err != nil {
					return err
				}
				hash, err := SkillTreeSHA256(backup)
				if err != nil || hash != entry.BeforeHash {
					return errors.New("restore skill copy differs from the native original")
				}
			}
		}
		point.Native = append(point.Native, entry)
	}
	encoded, err := json.MarshalIndent(point, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteChecked(path, append(encoded, '\n'), 0o600, func() error {
		if err := p.preflight(); err != nil {
			return err
		}
		return fileUnchanged(path, data, true)
	})
}

// RestorePlan reuses sync's preimage checks, atomic replacements, backups and
// rollback. Canonical data is archived rather than deleted, including user
// additions made since initialization.
type RestorePlan struct {
	Changes           []Change
	Archive           string
	plan              *Plan
	canonicalIdentity os.FileInfo
	point             []byte
	pointExists       bool
}

func BuildRestorePlan(home string) (*RestorePlan, error) {
	base, err := normalizeScopeRoot(home)
	if err != nil {
		return nil, err
	}
	canonical := filepath.Join(base, ".agentcfg")
	if err := ensureNoSymlinkParents(base, canonical); err != nil {
		return nil, err
	}
	identity, err := os.Lstat(canonical)
	if err != nil {
		return nil, err
	}
	pointPath := filepath.Join(canonical, initRestoreFile)
	if err := ensureNoSymlinkParents(base, pointPath); err != nil {
		return nil, err
	}
	data, exists, err := readOptional(pointPath)
	if err != nil {
		return nil, err
	}
	point := initRestorePoint{Version: 1}
	if !exists {
		// A canonical-only/partial setup never changed native files and can
		// be archived safely, including installations made before this feature.
		for _, name := range []string{"sync-user.json", "backups"} {
			if _, err := os.Lstat(filepath.Join(canonical, name)); !os.IsNotExist(err) {
				return nil, errors.New("no initialization restore point for these native changes: this installation predates restore support; original backups remain in ~/.agentcfg/backups")
			}
		}
	} else {
		if len(data) > 16<<20 {
			return nil, errors.New("initialization restore point is too large")
		}
		if err := json.Unmarshal(data, &point); err != nil || point.Version != 1 {
			return nil, errors.New("invalid initialization restore point")
		}
	}

	info, err := os.Lstat(base)
	if err != nil {
		return nil, err
	}
	p := &Plan{root: base, base: base, rootIdentity: info, baseIdentity: info,
		statePath: filepath.Join(canonical, "sync-user.json"), state: freshManifest()}
	p.stateBefore, p.stateExists, err = readOptional(p.statePath)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, entry := range point.Native {
		path, err := restoreTarget(base, entry.Path, entry.Skill)
		if err != nil {
			return nil, err
		}
		if known[path] {
			return nil, errors.New("duplicate initialization restore target")
		}
		known[path] = true
		change, err := restoreChange(p, path, entry)
		if err != nil {
			return nil, err
		}
		if change != nil {
			p.Changes = append(p.Changes, *change)
		}
	}
	if p.stateExists {
		var state manifest
		if err := json.Unmarshal(p.stateBefore, &state); err != nil {
			return nil, err
		}
		for path := range state.Configs {
			if !known[path] {
				return nil, fmt.Errorf("native configuration was synchronized after init without an original restore snapshot: %s", path)
			}
		}
		for path := range state.Skills {
			if !known[path] {
				return nil, fmt.Errorf("native skill was synchronized after init without an original restore snapshot: %s", path)
			}
		}
		if len(state.Instructions) > 0 {
			return nil, errors.New("instructions were synchronized after init; restore would leave managed files behind")
		}
	}
	return &RestorePlan{Changes: p.Changes, plan: p, point: data, pointExists: exists, canonicalIdentity: identity,
		Archive: filepath.Join(base, ".agentcfg-restored-"+time.Now().UTC().Format("20060102T150405.000000000"))}, nil
}

func restoreTarget(base, relative string, skill bool) (string, error) {
	if filepath.IsAbs(relative) || !withinScope(filepath.FromSlash(relative)) {
		return "", errors.New("invalid initialization restore path")
	}
	path := filepath.Join(base, filepath.FromSlash(relative))
	for _, target := range registry(base, "user") {
		if !skill && path == target.path || skill && filepath.Dir(path) == target.skills {
			return path, nil
		}
	}
	return "", fmt.Errorf("restore target is not a supported user harness location: %s", relative)
}

func restoreChange(p *Plan, path string, entry restoreEntry) (*Change, error) {
	parent := path
	if entry.Skill {
		parent = filepath.Dir(path)
	}
	if err := ensureNoSymlinkParents(p.base, parent); err != nil {
		return nil, err
	}
	if !entry.Skill {
		if entry.BeforeExists && rawDigest(entry.Before) != entry.BeforeHash {
			return nil, errors.New("damaged native file restore snapshot")
		}
		current, exists, err := readOptional(path)
		if err != nil {
			return nil, err
		}
		if exists == entry.BeforeExists && (!exists || bytes.Equal(current, entry.Before)) {
			return nil, nil
		}
		if exists == entry.AfterAbsent || exists && rawDigest(current) != entry.AfterHash {
			return nil, fmt.Errorf("native file changed since init; refusing to overwrite: %s", path)
		}
		return &Change{Path: path, Action: "restore pre-init native configuration", before: current, after: entry.Before, exists: exists, remove: !entry.BeforeExists}, nil
	}
	if !entry.AfterAbsent && filepath.Dir(entry.AfterSource) != filepath.Join(p.root, ".agentcfg", "skills") {
		return nil, errors.New("invalid canonical skill source in initialization restore point")
	}
	current, err := treeFingerprint(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	content := ""
	if exists {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil || filepath.Clean(link) != filepath.Clean(entry.AfterSource) {
				return nil, fmt.Errorf("native skill link changed since init: %s", path)
			}
			content, err = SkillTreeSHA256(entry.AfterSource)
			if err != nil {
				return nil, err
			}
		} else {
			content, err = SkillTreeSHA256(path)
			if err != nil {
				return nil, err
			}
			if entry.BeforeExists && content == entry.BeforeHash {
				return nil, nil
			}
		}
	}
	if !exists && !entry.BeforeExists {
		return nil, nil
	}
	if exists == entry.AfterAbsent || exists && content != entry.AfterHash {
		return nil, fmt.Errorf("native skill changed since init; refusing to overwrite: %s", path)
	}
	skill := &skillChange{beforeHash: current, mode: "copy", remove: !entry.BeforeExists}
	if entry.BeforeExists {
		relative := filepath.FromSlash(entry.Backup)
		if filepath.Clean(relative) != relative || filepath.Dir(relative) != filepath.Join("restore", "skills") {
			return nil, errors.New("invalid original skill backup path")
		}
		skill.source = filepath.Join(p.root, ".agentcfg", relative)
		if err := ensureNoSymlinkParents(p.root, skill.source); err != nil {
			return nil, err
		}
		hash, err := SkillTreeSHA256(skill.source)
		if err != nil || hash != entry.BeforeHash {
			return nil, errors.New("original skill backup changed or is missing")
		}
		skill.sourceHash, err = treeFingerprint(skill.source)
		if err != nil {
			return nil, err
		}
	}
	return &Change{Path: path, Action: "restore pre-init native skill", exists: exists, skill: skill}, nil
}

func (r *RestorePlan) Apply() error {
	canonical := filepath.Join(r.plan.root, ".agentcfg")
	if filepath.Dir(r.Archive) != r.plan.root || !strings.HasPrefix(filepath.Base(r.Archive), ".agentcfg-restored-") {
		return errors.New("restore archive must stay inside the user home")
	}
	check := func() error {
		if err := r.plan.checkScopeIdentity(); err != nil {
			return err
		}
		if err := ensureNoSymlinkParents(r.plan.root, filepath.Join(canonical, initRestoreFile)); err != nil {
			return err
		}
		if err := ensureNoSymlinkParents(r.plan.root, canonical); err != nil {
			return err
		}
		info, err := os.Lstat(canonical)
		if err != nil || !os.SameFile(info, r.canonicalIdentity) {
			return errors.New("canonical directory changed since restore preview")
		}
		return fileUnchanged(filepath.Join(canonical, initRestoreFile), r.point, r.pointExists)
	}
	if err := check(); err != nil {
		return err
	}
	r.plan.extraPreflight = check
	if err := r.plan.Apply(); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	if _, err := os.Lstat(r.Archive); !os.IsNotExist(err) {
		return errors.New("restore archive destination already exists")
	}
	if err := os.Rename(canonical, r.Archive); err != nil {
		return fmt.Errorf("native files restored, but canonical archive failed (retry restore): %w", err)
	}
	return nil
}
