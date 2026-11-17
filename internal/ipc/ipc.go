// Package ipc provides private per-user IPC endpoints. It never listens on TCP.
package ipc

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"agentcfg/internal/pathutil"
)

type Endpoint struct {
	Directory string
	Address   string
}

func ForRoot(root string) (Endpoint, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Endpoint{}, err
	}
	abs, err = pathutil.Resolve(abs)
	if err != nil {
		return Endpoint{}, err
	}
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	current, err := user.Current()
	if err != nil {
		return Endpoint{}, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(current.Uid+"\x00"+abs)))[:24]
	cache, err := os.UserCacheDir()
	if err != nil {
		return Endpoint{}, err
	}
	dir := filepath.Join(cache, "agentcfg", hash)
	address := filepath.Join(dir, "broker.sock")
	if runtime.GOOS == "windows" {
		address = `\\.\pipe\agentcfg-` + hash
	} else if len(address) > 100 {
		// Keep the established cache/fallback choice so existing brokers remain
		// reachable. TMPDIR itself can be long on macOS; bound that path too.
		dir = filepath.Join(os.TempDir(), "agentcfg-"+hash)
		address, err = socketAddress(dir, maxUnixSocketPath)
		if err != nil {
			return Endpoint{}, err
		}
	}
	return Endpoint{Directory: dir, Address: address}, nil
}

func socketAddress(directory string, limit int) (string, error) {
	address := filepath.Join(directory, "broker.sock")
	if len(address) <= limit {
		return address, nil
	}
	// Only an unusable legacy address changes. Retain the private directory,
	// full user/root identity and broker lifetime lock, even with a short name.
	address = filepath.Join(directory, "s")
	if len(address) > limit {
		return "", fmt.Errorf("temporary IPC path exceeds the Unix socket limit of %d bytes; set TMPDIR to a shorter directory", limit)
	}
	return address, nil
}

func (e Endpoint) Prepare() error {
	if info, err := os.Lstat(e.Directory); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		return fmt.Errorf("unsafe runtime directory %s", e.Directory)
	}
	if err := os.MkdirAll(e.Directory, 0o700); err != nil {
		return err
	}
	return secureDirectory(e.Directory)
}
