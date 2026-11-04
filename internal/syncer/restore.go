package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentcfg/internal/fsops"
)

const initRestoreFile = "init-restore.json"

type initRestorePoint struct {
	Version int                 `json:"version"`
	Native  []restoreEntry      `json:"native"`
	Layout  []nativeLayoutEntry `json:"layout,omitempty"`
}

type restoreEntry struct {
	Path         string `json:"path"` // relative to the user home
	Skill        bool   `json:"skill,omitempty"`
	BeforeExists bool   `json:"beforeExists"`
	Before       []byte `json:"before,omitempty"`
	BeforeHash   string `json:"beforeHash,omitempty"`
	BeforeLink   bool   `json:"beforeLink,omitempty"`
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
func (p *Plan) captureInitRestore(backupRoot string) (func() error, error) {
	if p.restoring || p.root != p.base || len(p.Changes) == 0 {
		return nil, nil
	}
	path := filepath.Join(p.root, ".agentcfg", initRestoreFile)
	if err := ensureNoSymlinkParents(p.root, path); err != nil {
		return nil, err
	}
	data, exists, err := readOptional(path)
	if err != nil || !exists {
		return nil, err
	}
	var point initRestorePoint
	if len(data) > 16<<20 || json.Unmarshal(data, &point) != nil || point.Version != 1 {
		return nil, errors.New("invalid initialization restore point")
	}
	for _, change := range p.Changes {
		relative, err := filepath.Rel(p.base, change.Path)
		if err != nil {
			return nil, err
		}
		if _, err := restoreTarget(p.base, filepath.ToSlash(relative), change.skill != nil); err != nil {
			continue
		}
		index := -1
		for i := range point.Native {
			if point.Native[i].Path == filepath.ToSlash(relative) {
				index = i
				break
			}
		}
		var previous *restoreEntry
		if index >= 0 {
			previous = &point.Native[index]
		}
		// A valid sync may preserve unrelated manual native edits. Do not adopt
		// those edits into an automatic restore permission; restore must still refuse.
		if previous != nil && change.skill == nil && (change.exists == previous.AfterAbsent || change.exists && rawDigest(change.before) != previous.AfterHash) {
			continue
		}
		entry, err := p.captureRestoreEntry(change, previous, backupRoot)
		if err != nil {
			return nil, err
		}
		if index < 0 {
			point.Native = append(point.Native, entry)
		} else {
			point.Native[index] = entry
		}
	}
	encoded, err := json.MarshalIndent(point, "", "  ")
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if len(encoded) > 16<<20 {
		return nil, errors.New("initialization restore point exceeds the byte limit")
	}
	if bytes.Equal(data, encoded) {
		return nil, nil
	}
	// Preserve the old journal alongside native backups for interrupted recovery.
	journalBackup := filepath.Join(backupRoot, initRestoreFile)
	if err := ensureNoSymlinkParents(p.root, journalBackup); err != nil {
		return nil, err
	}
	if err := atomicWriteChecked(journalBackup, data, 0600, func() error { return fileUnchanged(journalBackup, nil, false) }); err != nil {
		return nil, err
	}
	if err := atomicWriteChecked(path, encoded, 0600, func() error {
		if err := p.preflight(); err != nil {
			return err
		}
		return fileUnchanged(path, data, true)
	}); err != nil {
		return nil, err
	}
	return func() error {
		return atomicWriteChecked(path, data, 0600, func() error { return fileUnchanged(path, encoded, true) })
	}, nil
}

// Existing entries retain their original backup across the shared-root
// migration and later canonical edits; only the expected post-sync state is updated.
func (p *Plan) captureRestoreEntry(change Change, previous *restoreEntry, backupRoot string) (restoreEntry, error) {
	relative, err := filepath.Rel(p.base, change.Path)
	if err != nil || !withinScope(relative) {
		return restoreEntry{}, errors.New("restore target escapes user home")
	}
	entry := restoreEntry{Path: filepath.ToSlash(relative), BeforeExists: change.exists}
	if previous != nil {
		entry = *previous
	}
	entry.AfterAbsent = change.remove
	if change.skill == nil {
		if previous == nil {
			entry.Before = change.before
			entry.BeforeHash = rawDigest(change.before)
		}
		entry.AfterHash = rawDigest(change.after)
	} else {
		entry.Skill = true
		entry.AfterAbsent = change.skill.remove
		entry.AfterSource = change.skill.source
		if !entry.AfterAbsent {
			entry.AfterHash, err = SkillTreeSHA256(change.skill.source)
			if err != nil {
				return restoreEntry{}, err
			}
		}
		if previous == nil && change.exists {
			info, err := os.Lstat(change.Path)
			if err != nil {
				return restoreEntry{}, err
			}
			if redirected(info) {
				// Apply moves the original entry, preserving relative targets and
				// Windows junctions without requiring link-creation privileges.
				entry.BeforeLink = true
				entry.BeforeHash, err = treeFingerprint(change.Path)
				if err != nil {
					return restoreEntry{}, err
				}
				backup, err := filepath.Rel(filepath.Join(p.root, ".agentcfg"), changeBackupPath(backupRoot, change.Path))
				if err != nil || !withinScope(backup) {
					return restoreEntry{}, errors.New("original skill link backup escapes canonical directory")
				}
				entry.Backup = filepath.ToSlash(backup)
				return entry, nil
			}
			entry.Backup = filepath.ToSlash(filepath.Join("restore", "skills", rawDigest([]byte(relative))[:16]))
			backup := filepath.Join(p.root, ".agentcfg", filepath.FromSlash(entry.Backup))
			if err := ensureNoSymlinkParents(p.root, backup); err != nil {
				return restoreEntry{}, err
			}
			entry.BeforeHash, err = SkillTreeSHA256(change.Path)
			if err != nil {
				return restoreEntry{}, err
			}
			if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
				return restoreEntry{}, err
			}
			if err := copySkill(change.Path, backup); err != nil {
				return restoreEntry{}, err
			}
			hash, err := SkillTreeSHA256(backup)
			if err != nil || hash != entry.BeforeHash {
				return restoreEntry{}, errors.New("restore skill copy differs from the native original")
			}
		}
	}
	return entry, nil
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
	p := &Plan{restoring: true, root: base, base: base, rootIdentity: info, baseIdentity: info,
		statePath: filepath.Join(canonical, "sync-user.json"), state: freshManifest()}
	p.canonicalIdentity, err = os.Stat(canonical)
	if err != nil {
		return nil, err
	}
	p.stateBefore, p.stateExists, err = readOptional(p.statePath)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	if err := validateNativeLayout(base, point); err != nil {
		return nil, err
	}
	for _, entry := range point.Native {
		path, err := restoreTarget(base, entry.Path, entry.Skill)
		if err != nil {
			return nil, err
		}
		if known[path] {
			return nil, errors.New("duplicate initialization restore target")
		}
		known[path] = true
		alreadyRestored := false
		for _, layout := range point.Layout {
			if pathUnder(filepath.Join(base, filepath.FromSlash(layout.Path)), path) && layoutAlreadyRestored(base, layout) {
				alreadyRestored = true
				break
			}
		}
		if alreadyRestored {
			continue
		}
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
			if !known[path] && !layoutCovers(base, point.Layout, path) {
				return nil, fmt.Errorf("native configuration was synchronized after init without an original restore snapshot: %s", path)
			}
		}
		for path := range state.Skills {
			if !known[path] && !layoutCovers(base, point.Layout, path) {
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
	if skill && legacySkillPath(base, path) {
		return path, nil
	}
	if !skill {
		for _, candidate := range OpenCodeUserConfigPaths() {
			if path == filepath.Join(base, candidate) {
				return path, nil
			}
		}
	}
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
	if !entry.AfterAbsent && !filepath.IsAbs(entry.AfterSource) {
		return nil, errors.New("invalid canonical skill source in initialization restore point")
	}
	current, err := treeFingerprint(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if entry.BeforeLink && exists && current == entry.BeforeHash {
		return nil, nil // Includes an original relative link whose target is temporarily absent.
	}
	content := ""
	if exists {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if redirected(info) {
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
		if entry.BeforeLink {
			if filepath.Clean(relative) != relative || !withinScope(relative) ||
				!strings.HasPrefix(relative, "backups"+string(filepath.Separator)) {
				return nil, errors.New("invalid original skill link backup path")
			}
			skill.source = filepath.Join(p.root, ".agentcfg", relative)
			if err := ensureNoSymlinkParents(p.root, filepath.Dir(skill.source)); err != nil {
				return nil, err
			}
			info, err := os.Lstat(skill.source)
			if err != nil || !redirected(info) {
				return nil, errors.New("original skill link backup is missing or changed")
			}
			skill.sourceHash, err = treeFingerprint(skill.source)
			if err != nil || skill.sourceHash != entry.BeforeHash {
				return nil, errors.New("original skill link backup changed")
			}
			skill.restoreLink = true
			return &Change{Path: path, Action: "restore pre-init native skill link", exists: exists, skill: skill}, nil
		}
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
		skill.sourceHash, err = sourceTreeFingerprint(skill.source)
		if err != nil {
			return nil, err
		}
	}
	return &Change{Path: path, Action: "restore pre-init native skill", exists: exists, skill: skill}, nil
}

func changeBackupPath(root, path string) string {
	return filepath.Join(root, rawDigest([]byte(path))[:16]+"-"+filepath.Base(path))
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
	var point initRestorePoint
	if r.pointExists {
		if err := json.Unmarshal(r.point, &point); err != nil {
			return err
		}
	}
	if err := validateNativeLayout(r.plan.root, point); err != nil {
		return err
	}
	r.plan.extraPreflight = check
	if err := r.plan.Apply(); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	if err := restoreNativeLayout(r.plan.root, point); err != nil {
		return err
	}
	if _, err := os.Lstat(r.Archive); !os.IsNotExist(err) {
		return errors.New("restore archive destination already exists")
	}
	if err := fsops.Rename(context.Background(), canonical, r.Archive, nil); err != nil {
		return fmt.Errorf("native files restored, but canonical archive failed (retry restore): %w", err)
	}
	return nil
}
