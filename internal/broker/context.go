package broker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"
)

// Resolve cwd for this connection, not from the daemon's first launcher. The
// resolved Server is already part of acquire's key, so clients in the same
// directory share an upstream and different project contexts stay separate.
func withClientWorkingDirectory(entry config.Server, raw json.RawMessage) (config.Server, error) {
	if entry.Type != "stdio" || entry.Cwd != "" {
		return entry, nil
	}
	var cwd string
	if json.Unmarshal(raw, &cwd) != nil || !filepath.IsAbs(cwd) {
		return entry, fmt.Errorf("stdio connection requires the client's absolute working directory; reconnect with the updated agentcfg binary")
	}
	cwd, err := pathutil.Resolve(cwd)
	if err != nil {
		return entry, fmt.Errorf("client working directory: %w", err)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return entry, fmt.Errorf("client working directory: %w", err)
	}
	if !info.IsDir() {
		return entry, fmt.Errorf("client working directory must be a directory")
	}
	entry.Cwd = cwd
	return entry, nil
}
