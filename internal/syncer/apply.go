package syncer

import (
	"agentcfg/internal/config"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	return atomicWriteChecked(path, data, mode, nil)
}

func atomicWriteChecked(path string, data []byte, mode os.FileMode, check func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".agentcfg-write-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	return os.Rename(temp, path)
}

func fileUnchanged(path string, before []byte, existed bool) error {
	actual, exists, err := readOptional(path)
	if err != nil {
		return err
	}
	if exists != existed || !bytes.Equal(actual, before) {
		return fmt.Errorf("file changed since planning: %s", path)
	}
	return nil
}

func (p *Plan) checkCanonicalSource() error {
	if p.sourceConfigDigest != "" {
		latest, err := config.Load(p.root)
		if err != nil || digest(latest) != p.sourceConfigDigest {
			return fmt.Errorf("canonical MCP or harness configuration changed since planning; refresh preview")
		}
	}
	return nil
}

func (p *Plan) checkAdoptionSources() error {
	if p.adoption == nil {
		return nil
	}
	if err := p.recheckAdoptionScope(); err != nil {
		return err
	}
	if err := p.checkCanonicalSource(); err != nil {
		return err
	}
	for _, change := range p.Changes {
		if !change.adopted || change.skill == nil {
			continue
		}
		if err := ensureNoSymlinkParents(p.root, change.skill.source); err != nil {
			return err
		}
		current, err := SkillTreeSHA256(change.skill.source)
		if err != nil || current != change.skill.adoptionHash {
			return fmt.Errorf("canonical skill changed since adoption preview: %s", change.skill.source)
		}
	}
	return nil
}

// A matching spelling and matching bytes do not identify a replaced home.
// Preserve the directory identities captured by BuildPlan, including across
// alias spellings, before accepting any later path-based check.
func (p *Plan) checkScopeIdentity() error {
	for _, scope := range []struct {
		path string
		info os.FileInfo
	}{{p.root, p.rootIdentity}, {p.base, p.baseIdentity}} {
		current, err := os.Lstat(scope.path)
		if err != nil || scope.info == nil || !current.IsDir() || redirected(current) || !os.SameFile(scope.info, current) {
			return fmt.Errorf("scope root changed since planning: %s", scope.path)
		}
	}
	return nil
}

func (p *Plan) preflight() error {
	if p.extraPreflight != nil {
		if err := p.extraPreflight(); err != nil {
			return err
		}
	}
	if err := p.checkScopeIdentity(); err != nil {
		return err
	}
	if p.adoption == nil {
		if err := p.checkCanonicalSource(); err != nil {
			return err
		}
	} else {
		if err := p.checkAdoptionSources(); err != nil {
			return err
		}
	}
	if err := ensureNoSymlinkParents(p.root, p.statePath); err != nil {
		return err
	}
	if err := fileUnchanged(p.statePath, p.stateBefore, p.stateExists); err != nil {
		return err
	}
	for _, change := range p.Changes {
		if err := p.checkChange(change); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plan) checkChange(change Change) error {
	if p.extraPreflight != nil {
		if err := p.extraPreflight(); err != nil {
			return err
		}
	}
	if err := p.checkScopeIdentity(); err != nil {
		return err
	}
	if change.source != "" {
		if err := ensureNoSymlinkParents(p.root, change.source); err != nil {
			return err
		}
		contents, exists, err := readOptional(change.source)
		if err != nil {
			return err
		}
		if exists != change.sourceExists || exists && rawDigest(contents) != change.sourceHash {
			return fmt.Errorf("source instructions changed since planning: %s", change.source)
		}
	}
	path := change.Path
	if change.skill != nil {
		path = filepath.Dir(path)
	}
	if err := ensureNoSymlinkParents(p.base, path); err != nil {
		return err
	}
	if change.skill == nil {
		if change.adopted {
			return p.adoptedFileUnchanged(change.Path, change.before, change.exists)
		}
		return fileUnchanged(change.Path, change.before, change.exists)
	}
	fingerprint := treeFingerprint
	if change.adopted {
		fingerprint = adoptionTreeFingerprint
	}
	actual, err := fingerprint(change.Path)
	if os.IsNotExist(err) && !change.exists {
		err = nil
		actual = ""
	}
	if err != nil {
		return err
	}
	if actual != change.skill.beforeHash {
		return fmt.Errorf("skill changed since planning: %s", change.Path)
	}
	if change.adopted {
		content, err := SkillTreeSHA256(change.Path)
		if err != nil || content != change.skill.adoptionHash {
			return fmt.Errorf("native skill changed since adoption preview: %s", change.Path)
		}
	}
	if !change.skill.remove || change.adopted {
		sourceHash, err := fingerprint(change.skill.source)
		if err != nil {
			return err
		}
		if sourceHash != change.skill.sourceHash {
			return fmt.Errorf("source skill changed since planning: %s", change.skill.source)
		}
		if change.adopted {
			if err := ensureNoSymlinkParents(p.root, change.skill.source); err != nil {
				return err
			}
			content, err := SkillTreeSHA256(change.skill.source)
			if err != nil || content != change.skill.adoptionHash {
				return fmt.Errorf("source skill changed since adoption preview: %s", change.skill.source)
			}
		}
	}
	return nil
}

// Apply checks every preimage before writing, locks the target scope, backs up
// modified content, uses atomic replacements and rolls back on an I/O failure.
func (p *Plan) Apply() error {
	if err := p.recheckAdoptionScope(); err != nil {
		return err
	}
	if err := p.checkScopeIdentity(); err != nil {
		return err
	}
	if len(p.Changes) == 0 {
		encoded, err := json.MarshalIndent(p.state, "", "  ")
		if err != nil {
			return err
		}
		encoded = append(encoded, '\n')
		empty := !p.stateExists && p.state.Version == 1 && len(p.state.Configs)+len(p.state.Skills)+len(p.state.Blocks)+len(p.state.Instructions) == 0
		if empty || bytes.Equal(encoded, p.stateBefore) {
			// Validate a no-op without creating a directory or lock. A manifest
			// update still takes the regular locked path, even with no changes.
			return p.preflight()
		}
	}
	lockDir := filepath.Join(p.base, ".agentcfg")
	lockPath := filepath.Join(lockDir, "sync.lock")
	if err := ensureNoSymlinkParents(p.base, lockPath); err != nil {
		return err
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return err
	}
	if err := ensureNoSymlinkParents(p.base, lockPath); err != nil {
		return err
	}
	lock := flock.New(lockPath)
	defer lock.Close()
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("another sync is already applying changes to this scope")
	}
	if err := p.preflight(); err != nil {
		return err
	}
	if err := p.captureInitRestore(); err != nil {
		return err
	}
	backupRoot := filepath.Join(lockDir, "backups", time.Now().UTC().Format("20060102T150405.000000000"))
	if err := ensureNoSymlinkParents(p.base, backupRoot); err != nil {
		return err
	}
	undos := []func() error{}
	rollback := func(cause error) error {
		var failures []error
		for i := len(undos) - 1; i >= 0; i-- {
			if err := undos[i](); err != nil {
				failures = append(failures, err)
			}
		}
		if len(failures) != 0 {
			return errors.Join(append([]error{fmt.Errorf("apply failed (%w); rollback incomplete, backups: %s", cause, backupRoot)}, failures...)...)
		}
		return fmt.Errorf("apply failed and changes were rolled back: %w", cause)
	}
	for _, change := range p.Changes {
		if err := p.checkChange(change); err != nil {
			return rollback(err)
		}
		backup := filepath.Join(backupRoot, rawDigest([]byte(change.Path))[:16]+"-"+filepath.Base(change.Path))
		undo, err := p.applyChange(change, backup)
		if undo != nil {
			undos = append(undos, undo)
		}
		if err != nil {
			return rollback(err)
		}
	}
	if err := p.checkAdoptionSources(); err != nil {
		return rollback(err)
	}
	encoded, err := json.MarshalIndent(p.state, "", "  ")
	if err != nil {
		return rollback(err)
	}
	encoded = append(encoded, '\n')
	if bytes.Equal(encoded, p.stateBefore) {
		return nil
	}
	checkState := func() error {
		if err := p.checkScopeIdentity(); err != nil {
			return err
		}
		if err := ensureNoSymlinkParents(p.root, p.statePath); err != nil {
			return err
		}
		return fileUnchanged(p.statePath, p.stateBefore, p.stateExists)
	}
	if err := checkState(); err != nil {
		return rollback(err)
	}
	if err := atomicWriteChecked(p.statePath, encoded, 0o600, checkState); err != nil {
		return rollback(err)
	}
	return nil
}

func (p *Plan) applyChange(change Change, backup string) (func() error, error) {
	if change.skill != nil {
		return p.applySkill(change, backup)
	}
	check := func() error {
		if err := p.checkAdoptionSources(); err != nil {
			return err
		}
		return p.checkChange(change)
	}
	mode := os.FileMode(0o600)
	if change.exists {
		info, err := os.Stat(change.Path)
		if err != nil {
			return nil, err
		}
		mode = info.Mode().Perm()
		if err := atomicWrite(backup, change.before, 0o600); err != nil {
			return nil, err
		}
	}
	if change.remove {
		if err := check(); err != nil {
			return nil, err
		}
		if change.exists {
			if err := os.Remove(change.Path); err != nil {
				return nil, err
			}
		}
		return func() error {
			check := func() error {
				if err := p.checkScopeIdentity(); err != nil {
					return err
				}
				if err := ensureNoSymlinkParents(p.base, change.Path); err != nil {
					return err
				}
				return fileUnchanged(change.Path, nil, false)
			}
			if err := check(); err != nil {
				return err
			}
			if change.exists {
				return atomicWriteChecked(change.Path, change.before, mode, check)
			}
			return nil
		}, nil
	}
	if err := atomicWriteChecked(change.Path, change.after, mode, check); err != nil {
		return nil, err
	}
	return func() error {
		check := func() error {
			if err := p.checkScopeIdentity(); err != nil {
				return err
			}
			if err := ensureNoSymlinkParents(p.base, change.Path); err != nil {
				return err
			}
			if change.adopted {
				return p.adoptedFileUnchanged(change.Path, change.after, true)
			}
			return fileUnchanged(change.Path, change.after, true)
		}
		if err := check(); err != nil {
			return err
		}
		if change.exists {
			return atomicWriteChecked(change.Path, change.before, mode, check)
		}
		return os.Remove(change.Path)
	}, nil
}

func (p *Plan) applySkill(change Change, backup string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(change.Path), 0o700); err != nil {
		return nil, err
	}
	staged, mode := "", change.skill.mode
	installedHash := ""
	if !change.skill.remove {
		var err error
		staged, err = os.MkdirTemp(filepath.Dir(change.Path), ".agentcfg-stage-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(staged)
		if err := os.Remove(staged); err != nil {
			return nil, err
		}
		if mode != "copy" {
			err = os.Symlink(change.skill.source, staged)
			if err == nil {
				mode = "symlink"
			} else if mode == "symlink" {
				return nil, err
			} else {
				mode = "copy"
			}
		}
		if mode == "copy" {
			var err error
			if change.adopted {
				var copied string
				copied, _, err = skillTreeHashes(change.skill.source, staged)
				if err == nil && copied != change.skill.adoptionHash {
					err = errors.New("copied skill differs from the approved import")
				}
			} else {
				err = copySkill(change.skill.source, staged)
			}
			if err != nil {
				return nil, err
			}
		}
		sourceFingerprint := treeFingerprint
		if change.adopted {
			sourceFingerprint = adoptionTreeFingerprint
		}
		currentSource, err := sourceFingerprint(change.skill.source)
		if err != nil || currentSource != change.skill.sourceHash {
			return nil, fmt.Errorf("source skill changed during staging: %s", change.skill.source)
		}
		if change.adopted && mode == "copy" {
			content, err := SkillTreeSHA256(staged)
			if err != nil || content != change.skill.adoptionHash {
				return nil, fmt.Errorf("staged skill differs from the approved import: %s", change.Path)
			}
		}
		stagedFingerprint := treeFingerprint
		if change.adopted && mode == "copy" {
			stagedFingerprint = adoptionTreeFingerprint
		}
		installedHash, err = stagedFingerprint(staged)
		if err != nil {
			return nil, err
		}
	}
	if change.adopted {
		if err := p.checkAdoptionSources(); err != nil {
			return nil, err
		}
	}
	// Copying can take time. Recheck every native tree immediately before
	// moving it into the backup, including adoption proofs when applicable.
	if err := p.checkChange(change); err != nil {
		return nil, err
	}
	if change.exists {
		if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
			return nil, err
		}
		if err := os.Rename(change.Path, backup); err != nil {
			return nil, err
		}
	}
	installed := false
	installedFingerprint := treeFingerprint
	if change.adopted && mode == "copy" {
		installedFingerprint = adoptionTreeFingerprint
	}
	undo := func() error {
		if err := p.checkScopeIdentity(); err != nil {
			return err
		}
		if err := ensureNoSymlinkParents(p.base, filepath.Dir(change.Path)); err != nil {
			return err
		}
		if err := ensureNoSymlinkParents(p.base, filepath.Dir(backup)); err != nil {
			return err
		}
		if installed {
			current, err := installedFingerprint(change.Path)
			if err != nil {
				return err
			}
			if current != installedHash {
				return fmt.Errorf("cannot roll back externally edited skill: %s", change.Path)
			}
			if change.adopted && mode == "copy" {
				content, err := SkillTreeSHA256(change.Path)
				if err != nil || content != change.skill.adoptionHash {
					return fmt.Errorf("cannot roll back externally edited adopted skill: %s", change.Path)
				}
			}
			if err := os.RemoveAll(change.Path); err != nil {
				return err
			}
		} else if _, err := os.Lstat(change.Path); !os.IsNotExist(err) {
			if err != nil {
				return err
			}
			return fmt.Errorf("cannot restore skill backup over a new destination: %s", change.Path)
		}
		if change.exists {
			return os.Rename(backup, change.Path)
		}
		return nil
	}
	if staged != "" {
		if err := os.Rename(staged, change.Path); err != nil {
			return undo, err
		}
		installed = true
	}
	if !change.skill.remove {
		actual, err := installedFingerprint(change.Path)
		if err != nil || actual != installedHash {
			return undo, fmt.Errorf("installed skill changed during replacement: %s", change.Path)
		}
		record := p.state.Skills[change.Path]
		record.Fingerprint, record.Mode = installedHash, mode
		p.state.Skills[change.Path] = record
	}
	return undo, nil
}
