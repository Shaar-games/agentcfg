package syncer

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/fsstate"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
)

// Recognition deliberately does not depend on the current gateway settings or
// key: a disabled or relocated gateway can still have capabilities in backups.
var gatewayCapabilityPath = regexp.MustCompile(`/mcp/[A-Za-z0-9_-]{43}/`)

func gatewayValueContainsCapability(value any) bool {
	switch value := value.(type) {
	case string:
		return gatewayCapabilityPath.MatchString(value)
	case []any:
		for _, item := range value {
			if gatewayValueContainsCapability(item) {
				return true
			}
		}
	case map[string]any:
		for key, item := range value {
			if gatewayCapabilityPath.MatchString(key) || gatewayValueContainsCapability(item) {
				return true
			}
		}
	}
	return false
}

func nativeGatewayCapability(data []byte) bool {
	if gatewayCapabilityPath.Match(data) {
		return true
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if standard, err := hujson.Standardize(bytes.Clone(data)); err == nil {
		var value any
		if json.Unmarshal(standard, &value) == nil && gatewayValueContainsCapability(value) {
			return true
		}
	}
	var document map[string]any
	return toml.Unmarshal(data, &document) == nil && gatewayValueContainsCapability(document)
}

func containsGatewayCapability(data []byte) bool {
	if nativeGatewayCapability(data) {
		return true
	}
	// Restore journals encode the original native bytes as base64. Decode only
	// that known structure, then apply the same JSON/JSONC/TOML recognition.
	var point initRestorePoint
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if json.Unmarshal(data, &point) == nil {
		for _, entry := range point.Native {
			if nativeGatewayCapability(entry.Before) {
				return true
			}
		}
	}
	return false
}

func checkGatewayDataPrivacy(path string, data []byte) error {
	if containsGatewayCapability(data) {
		return auth.CheckGatewayConfigPrivacy(path)
	}
	return nil
}

func checkGatewayLayoutCopy(_, destination string, data []byte, mode os.FileMode) error {
	if !containsGatewayCapability(data) {
		return nil
	}
	return auth.CheckGatewayConfigCopyPrivacy(destination, mode)
}

func checkGatewayLayoutMove(source, destination string, snapshot *fsstate.Snapshot) error {
	for relative, expected := range snapshot.Files {
		data, err := fsstate.ReadFile(filepath.Join(source, filepath.FromSlash(relative)), "", 64<<20)
		if err != nil {
			return err
		}
		if rawDigest(data) != expected {
			return errors.New("native layout contents changed while checking gateway privacy")
		}
		if containsGatewayCapability(data) {
			if err := auth.CheckGatewayConfigMovePrivacy(source, destination, relative); err != nil {
				return err
			}
		}
	}
	return nil
}

// Check every recovery destination before the first native or journal change.
// atomicWriteChecked repeats these checks before any sensitive bytes are written.
func (p *Plan) checkGatewayRecoveryPrivacy(backupRoot string) error {
	if len(p.Changes) == 0 {
		return nil
	}
	if backupRoot == "" {
		backupRoot = filepath.Join(p.base, ".agentcfg", "backups", time.Now().UTC().Format("20060102T150405.000000000"))
	}
	for _, change := range p.Changes {
		if change.skill != nil {
			continue
		}
		if err := checkGatewayDataPrivacy(change.Path, change.after); err != nil {
			return err
		}
		if change.exists {
			if err := checkGatewayDataPrivacy(changeBackupPath(backupRoot, change.Path), change.before); err != nil {
				return err
			}
		}
	}
	if p.restoring || p.root != p.base {
		return nil
	}
	data, exists, err := readOptional(filepath.Join(p.root, ".agentcfg", initRestoreFile))
	if err != nil || !exists {
		return err
	}
	var point initRestorePoint
	if json.Unmarshal(data, &point) != nil || point.Version != 1 {
		// captureInitRestore reports invalid journals before writing them.
		return nil
	}
	return p.checkInitRestoreGatewayPrivacy(backupRoot, data, point)
}

func (p *Plan) checkInitRestoreGatewayPrivacy(backupRoot string, data []byte, point initRestorePoint) error {
	previousSensitive := containsGatewayCapability(data)
	nextSensitive := previousSensitive
	known := make(map[string]bool, len(point.Native))
	for _, entry := range point.Native {
		known[entry.Path] = true
	}
	for _, change := range p.Changes {
		if change.skill != nil || !containsGatewayCapability(change.before) {
			continue
		}
		relative, err := filepath.Rel(p.base, change.Path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if _, err := restoreTarget(p.base, relative, false); err == nil && !known[relative] {
			nextSensitive = true
		}
	}
	if nextSensitive {
		if err := auth.CheckGatewayConfigPrivacy(filepath.Join(p.root, ".agentcfg", initRestoreFile)); err != nil {
			return err
		}
	}
	if previousSensitive {
		return auth.CheckGatewayConfigPrivacy(filepath.Join(backupRoot, initRestoreFile))
	}
	return nil
}
