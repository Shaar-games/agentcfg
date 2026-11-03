package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
	"unicode/utf8"

	"agentmux/internal/config"
	"agentmux/internal/pathutil"

	"github.com/gofrs/flock"
)

const maxSettingsBytes = 1 << 20

func settingsFile(root string) string { return filepath.Join(root, ".agentmux", "agentmux.json") }

// setHarnessEnabled changes the canonical user-scope harness selection. An
// expected state check refuses stale edits, while a lock protects concurrent
// AgentMux TUI writers. Other settings keys are retained verbatim as values.
func setHarnessEnabled(root, harness string, enabled, expected bool) error {
	valid := false
	for _, info := range harnesses {
		if info.ID == harness && info.UserScopeEnabled {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("%s cannot be managed in user scope", harness)
	}
	return updateSettings(root, func(cfg *config.Config, fields map[string]json.RawMessage) error {
		current := slices.Contains(cfg.Settings.Harnesses, harness)
		if current != expected {
			return fmt.Errorf("%s was modified elsewhere; refresh before retrying", harness)
		}
		if current == enabled {
			return errNoChange
		}
		selected := make([]string, 0, len(cfg.Settings.Harnesses)+1)
		for _, name := range cfg.Settings.Harnesses {
			if name != harness {
				selected = append(selected, name)
			}
		}
		if enabled {
			selected = append(selected, harness)
		}
		value, err := json.Marshal(selected)
		if err != nil {
			return err
		}
		fields["harnesses"] = value
		return nil
	})
}

// setServerSharing changes the effective shared/isolated mode using a validated
// server override; it leaves the source/plugin MCP definitions untouched.
func setServerSharing(root, name, next, expected string) error {
	if next != "shared" && next != "isolated" {
		return fmt.Errorf("unsupported sharing mode %q", next)
	}
	return updateSettings(root, func(cfg *config.Config, fields map[string]json.RawMessage) error {
		server, exists := cfg.Servers[name]
		if !exists {
			return fmt.Errorf("MCP server %q no longer exists", name)
		}
		current := sharing(server)
		if current != expected {
			return fmt.Errorf("sharing for %q was modified elsewhere; refresh before retrying", name)
		}
		if current == next {
			return errNoChange
		}
		overrides := make(map[string]json.RawMessage)
		if raw := fields["serverOverrides"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &overrides); err != nil || overrides == nil {
				return fmt.Errorf("serverOverrides is not a JSON object")
			}
		}
		item := make(map[string]json.RawMessage)
		if raw := overrides[name]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &item); err != nil || item == nil {
				return fmt.Errorf("server override %q is not a JSON object", name)
			}
		}
		item["sharing"], _ = json.Marshal(next)
		var err error
		overrides[name], err = json.Marshal(item)
		if err != nil {
			return err
		}
		fields["serverOverrides"], err = json.Marshal(overrides)
		return err
	})
}

var errNoChange = errors.New("requested setting already has that value")

// updateSettings performs a checked, backup-first atomic edit to the source
// agentmux.json. It never creates or updates project/harness configuration.
func updateSettings(root string, mutate func(*config.Config, map[string]json.RawMessage) error) error {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	agentDir := filepath.Join(absolute, ".agentmux")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return fmt.Errorf("prepare agentmux settings directory: %w", err)
	}
	info, err := os.Lstat(agentDir)
	if err != nil {
		return err
	}
	if !info.IsDir() || pathutil.IsRedirected(info) {
		return fmt.Errorf("settings directory must be a real directory within %s", absolute)
	}
	// Serialize edits with syncer.Plan.Apply, which projects these canonical
	// settings while holding the same lock. Separate TUI-only locks allowed
	// a source change in the middle of a multi-file synchronization.
	lock := flock.New(filepath.Join(agentDir, "sync.lock"))
	if err := rejectLinkedFile(filepath.Join(agentDir, "sync.lock")); err != nil {
		return err
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 35*time.Millisecond)
	if err != nil {
		return fmt.Errorf("settings lock: %w", err)
	}
	if !locked {
		return errors.New("settings are locked by another operation; refresh and retry")
	}
	defer lock.Unlock()

	cfg, err := config.Load(absolute)
	if err != nil {
		return fmt.Errorf("settings validation: %w", err)
	}
	path := settingsFile(absolute)
	original, existed, err := readSettings(path)
	if err != nil {
		return err
	}
	fields := make(map[string]json.RawMessage)
	if existed {
		if err := json.Unmarshal(bytes.TrimPrefix(original, []byte{0xef, 0xbb, 0xbf}), &fields); err != nil || fields == nil {
			return errors.New("settings file must be a valid JSON object")
		}
	}
	if err := mutate(cfg, fields); err != nil {
		return err
	}

	updated, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	updated = append(updated, '\n')
	if bytes.HasPrefix(original, []byte{0xef, 0xbb, 0xbf}) {
		updated = append([]byte{0xef, 0xbb, 0xbf}, updated...)
	}
	if len(updated) > maxSettingsBytes {
		return fmt.Errorf("settings exceed %d bytes", maxSettingsBytes)
	}
	if bytes.Equal(original, updated) {
		return nil
	}

	// Do not overwrite user changes made during preparation; AgentMux writers
	// coordinate using this lock and uncoordinated edits are checked again.
	latest, stillExists, err := readSettings(path)
	if err != nil {
		return err
	}
	if stillExists != existed || !bytes.Equal(latest, original) {
		return errors.New("settings changed during edit; refresh before retrying")
	}

	temporary, err := os.CreateTemp(agentDir, ".agentmux-settings-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary settings: %w", err)
	}
	tempName := temporary.Name()
	defer os.Remove(tempName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(updated); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}

	if existed {
		backup := path + ".bak-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		if err := writeBackup(backup, original); err != nil {
			return fmt.Errorf("back up settings: %w", err)
		}
	}
	latest, stillExists, err = readSettings(path)
	if err != nil {
		return err
	}
	if stillExists != existed || !bytes.Equal(latest, original) {
		return errors.New("settings changed before replacement; refresh before retrying")
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("atomically replace settings: %w", err)
	}
	return nil
}

func readSettings(path string) (data []byte, exists bool, err error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSettingsBytes {
		return nil, false, fmt.Errorf("settings must be a regular file of at most %d bytes", maxSettingsBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	data, err = io.ReadAll(io.LimitReader(file, maxSettingsBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > maxSettingsBytes || !utf8.Valid(data) {
		return nil, false, errors.New("settings must be UTF-8 and smaller than 1 MiB")
	}
	return data, true, nil
}

func writeBackup(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err = file.Close(); err != nil {
		_ = os.Remove(path)
	}
	return err
}
