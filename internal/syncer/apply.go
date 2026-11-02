package syncer

import (
	"agentmux/internal/config"
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".agentmux-write-*")
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

func (p *Plan) preflight() error {
	if p.sourceConfigDigest != "" {
		latest, err := config.Load(p.root)
		if err != nil || digest(latest) != p.sourceConfigDigest {
			return fmt.Errorf("canonical MCP or harness configuration changed since planning; refresh preview")
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
		return fileUnchanged(change.Path, change.before, change.exists)
	}
	actual, err := treeFingerprint(change.Path)
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
	if !change.skill.remove {
		sourceHash, err := treeFingerprint(change.skill.source)
		if err != nil {
			return err
		}
		if sourceHash != change.skill.sourceHash {
			return fmt.Errorf("source skill changed since planning: %s", change.skill.source)
		}
	}
	return nil
}

// Apply checks every preimage before writing, locks the target scope, backs up
// modified content, uses atomic replacements and rolls back on an I/O failure.
func (p *Plan) Apply() error {
	lockDir := filepath.Join(p.base, ".agentmux")
	if err := ensureNoSymlinkParents(p.base, lockDir); err != nil {
		return err
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(lockDir, "sync.lock"))
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
		if err != nil {
			return rollback(err)
		}
		undos = append(undos, undo)
	}
	encoded, err := json.MarshalIndent(p.state, "", "  ")
	if err != nil {
		return rollback(err)
	}
	encoded = append(encoded, '\n')
	if bytes.Equal(encoded, p.stateBefore) {
		return nil
	}
	if err := fileUnchanged(p.statePath, p.stateBefore, p.stateExists); err != nil {
		return rollback(err)
	}
	if err := atomicWrite(p.statePath, encoded, 0o600); err != nil {
		return rollback(err)
	}
	return nil
}

func (p *Plan) applyChange(change Change, backup string) (func() error, error) {
	if change.skill != nil {
		return p.applySkill(change, backup)
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
		if change.exists {
			if err := os.Remove(change.Path); err != nil {
				return nil, err
			}
		}
		return func() error {
			if err := fileUnchanged(change.Path, nil, false); err != nil {
				return err
			}
			if change.exists {
				return atomicWrite(change.Path, change.before, mode)
			}
			return nil
		}, nil
	}
	if err := atomicWrite(change.Path, change.after, mode); err != nil {
		return nil, err
	}
	return func() error {
		if err := fileUnchanged(change.Path, change.after, true); err != nil {
			return err
		}
		if change.exists {
			return atomicWrite(change.Path, change.before, mode)
		}
		return os.Remove(change.Path)
	}, nil
}

func (p *Plan) applySkill(change Change, backup string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(change.Path), 0o700); err != nil {
		return nil, err
	}
	staged, mode := "", change.skill.mode
	if !change.skill.remove {
		var err error
		staged, err = os.MkdirTemp(filepath.Dir(change.Path), ".agentmux-stage-*")
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
			if err := copySkill(change.skill.source, staged); err != nil {
				return nil, err
			}
		}
		currentSource, err := treeFingerprint(change.skill.source)
		if err != nil || currentSource != change.skill.sourceHash {
			return nil, fmt.Errorf("source skill changed during staging: %s", change.skill.source)
		}
	}
	if change.exists {
		if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
			return nil, err
		}
		if err := os.Rename(change.Path, backup); err != nil {
			return nil, err
		}
	}
	if staged != "" {
		if err := os.Rename(staged, change.Path); err != nil {
			if change.exists {
				_ = os.Rename(backup, change.Path)
			}
			return nil, err
		}
	}
	installedHash := ""
	if !change.skill.remove {
		var err error
		installedHash, err = treeFingerprint(change.Path)
		if err != nil {
			_ = os.RemoveAll(change.Path)
			if change.exists {
				_ = os.Rename(backup, change.Path)
			}
			return nil, err
		}
		record := p.state.Skills[change.Path]
		record.Fingerprint, record.Mode = installedHash, mode
		p.state.Skills[change.Path] = record
	}
	return func() error {
		current, err := treeFingerprint(change.Path)
		if change.skill.remove && os.IsNotExist(err) {
			err = nil
			current = ""
		}
		if err != nil {
			return err
		}
		if current != installedHash {
			return fmt.Errorf("cannot roll back externally edited skill: %s", change.Path)
		}
		if !change.skill.remove {
			if err := os.RemoveAll(change.Path); err != nil {
				return err
			}
		}
		if change.exists {
			return os.Rename(backup, change.Path)
		}
		return nil
	}, nil
}
