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
	"reflect"
	"slices"
	"time"
	"unicode/utf8"

	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"

	"github.com/gofrs/flock"
)

const maxSettingsBytes = 1 << 20

func settingsFile(root string) string { return filepath.Join(root, ".agentcfg", "agentcfg.json") }

// setHarnessEnabled changes the canonical user-scope harness selection. An
// expected state check refuses stale edits, while a lock protects concurrent
// agentcfg TUI writers. Other settings keys are retained verbatim as values.
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

// setHarnessSelection applies the entire Huh multi-select as one atomic edit.
// The UI's snapshot is revalidated under the sync lock, avoiding a sequence of
// partial per-harness toggles if the selection has changed elsewhere.
func setHarnessSelection(root string, selected, expected []string) error {
	validated, err := config.ValidateUserHarnesses(selected)
	if err != nil {
		return err
	}
	return updateSettings(root, func(cfg *config.Config, fields map[string]json.RawMessage) error {
		if !slices.Equal(cfg.Settings.Harnesses, expected) {
			return errors.New("harness selection changed elsewhere; refresh before editing")
		}
		desired := make(map[string]bool, len(validated))
		for _, name := range validated {
			desired[name] = true
		}
		current := cfg.Settings.Harnesses
		allSame := len(validated) == len(current)
		for _, name := range current {
			allSame = allSame && desired[name]
		}
		if allSame {
			return errNoChange
		}
		// Keep the original relative ordering of still-enabled harnesses.
		merged := make([]string, 0, len(validated))
		for _, name := range current {
			if desired[name] {
				merged = append(merged, name)
				delete(desired, name)
			}
		}
		for _, name := range validated {
			if desired[name] {
				merged = append(merged, name)
				delete(desired, name)
			}
		}
		fields["harnesses"], err = json.Marshal(merged)
		return err
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

// setServerHarnessSelection edits just the named MCP's per-harness routing in
// canonical serverOverrides. Huh supplies an atomic selection for currently
// configured harnesses; filters for other (currently disabled) harnesses are
// retained. A "none" sentinel represents an explicitly disabled MCP.
func setServerHarnessSelection(root string, snapshot config.Server, chosen, configured []string) error {
	configured, err := config.ValidateUserHarnesses(configured)
	if err != nil {
		return err
	}
	selected := make(map[string]bool, len(chosen))
	for _, name := range chosen {
		if !slices.Contains(configured, name) || selected[name] {
			return fmt.Errorf("invalid or repeated MCP harness %q", name)
		}
		selected[name] = true
	}
	return updateSettings(root, func(cfg *config.Config, fields map[string]json.RawMessage) error {
		if !slices.Equal(cfg.Settings.Harnesses, configured) {
			return errors.New("configured harnesses changed; refresh the MCP checklist")
		}
		server, exists := cfg.Servers[snapshot.Name]
		// Availability approval belongs to the server shown by the checklist.
		// A concurrent sharing-only edit is independent and is preserved below.
		definition, previous := server, snapshot
		definition.Sharing, previous.Sharing = "", ""
		if !exists || !reflect.DeepEqual(definition, previous) {
			return fmt.Errorf("MCP %q changed since checklist opened; refresh", snapshot.Name)
		}
		unchanged := true
		for _, name := range configured {
			if config.Allows(server.Harnesses, name) != selected[name] {
				unchanged = false
			}
		}
		if unchanged {
			return errNoChange
		}
		// No filter means all harnesses; "none" means explicitly none.
		allowed := make(map[string]bool, len(knownSkillHarnesses))
		if len(server.Harnesses) == 0 {
			for _, name := range knownSkillHarnesses {
				allowed[name] = true
			}
		} else {
			for _, name := range server.Harnesses {
				if name != "none" {
					allowed[name] = true
				}
			}
		}
		for _, name := range configured {
			allowed[name] = selected[name]
		}
		filter := make([]string, 0, len(knownSkillHarnesses))
		for _, name := range knownSkillHarnesses {
			if allowed[name] {
				filter = append(filter, name)
			}
		}
		if len(filter) == 0 {
			filter = []string{"none"}
		}
		overrides := make(map[string]json.RawMessage)
		if raw := fields["serverOverrides"]; len(raw) != 0 {
			if err := json.Unmarshal(raw, &overrides); err != nil || overrides == nil {
				return errors.New("serverOverrides must be a JSON object")
			}
		}
		item := make(map[string]json.RawMessage)
		if raw := overrides[snapshot.Name]; len(raw) != 0 {
			if err := json.Unmarshal(raw, &item); err != nil || item == nil {
				return fmt.Errorf("server override %q is not a JSON object", snapshot.Name)
			}
		}
		item["harnesses"], err = json.Marshal(filter)
		if err != nil {
			return err
		}
		overrides[snapshot.Name], err = json.Marshal(item)
		if err != nil {
			return err
		}
		fields["serverOverrides"], err = json.Marshal(overrides)
		return err
	})
}

var errNoChange = errors.New("requested setting already has that value")

// updateSettings performs a checked, backup-first atomic edit to the source
// agentcfg.json. It never creates or updates project/harness configuration.
func updateSettings(root string, mutate func(*config.Config, map[string]json.RawMessage) error) error {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	agentDir := filepath.Join(absolute, ".agentcfg")
	if info, err := os.Lstat(agentDir); err == nil {
		if !info.IsDir() || pathutil.IsRedirected(info) {
			return fmt.Errorf("settings directory must be a real directory within %s", absolute)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// Reject stale/invalid requests and no-ops before creating even a lock file.
	// The mutation is prepared again after locking, against a fresh snapshot.
	if _, _, _, err := prepareSettingsEdit(absolute, mutate); err != nil {
		return err
	}
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return fmt.Errorf("prepare agentcfg settings directory: %w", err)
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

	path := settingsFile(absolute)
	original, updated, existed, err := prepareSettingsEdit(absolute, mutate)
	if err != nil {
		return err
	}

	// Do not overwrite user changes made during preparation; agentcfg writers
	// coordinate using this lock and uncoordinated edits are checked again.
	latest, stillExists, err := readSettings(path)
	if err != nil {
		return err
	}
	if stillExists != existed || !bytes.Equal(latest, original) {
		return errors.New("settings changed during edit; refresh before retrying")
	}

	temporary, err := os.CreateTemp(agentDir, ".agentcfg-settings-*.tmp")
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

// prepareSettingsEdit is read-only; mutate must only edit its in-memory fields.
// Bracket config.Load with the raw preimage so parsed checks and preserved JSON
// cannot silently refer to different settings versions.
func prepareSettingsEdit(root string, mutate func(*config.Config, map[string]json.RawMessage) error) ([]byte, []byte, bool, error) {
	path := settingsFile(root)
	original, existed, err := readSettings(path)
	if err != nil {
		return nil, nil, false, err
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil, nil, false, fmt.Errorf("settings validation: %w", err)
	}
	latest, stillExists, err := readSettings(path)
	if err != nil {
		return nil, nil, false, err
	}
	if stillExists != existed || !bytes.Equal(latest, original) {
		return nil, nil, false, errors.New("settings changed while loading; refresh before retrying")
	}
	fields := make(map[string]json.RawMessage)
	if existed {
		if err := json.Unmarshal(bytes.TrimPrefix(original, []byte{0xef, 0xbb, 0xbf}), &fields); err != nil || fields == nil {
			return nil, nil, false, errors.New("settings file must be a valid JSON object")
		}
	}
	if err := mutate(cfg, fields); err != nil {
		return nil, nil, false, err
	}
	updated, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return nil, nil, false, err
	}
	updated = append(updated, '\n')
	if bytes.HasPrefix(original, []byte{0xef, 0xbb, 0xbf}) {
		updated = append([]byte{0xef, 0xbb, 0xbf}, updated...)
	}
	if len(updated) > maxSettingsBytes {
		return nil, nil, false, fmt.Errorf("settings exceed %d bytes", maxSettingsBytes)
	}
	if bytes.Equal(original, updated) {
		return nil, nil, false, errNoChange
	}
	return original, updated, existed, nil
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
