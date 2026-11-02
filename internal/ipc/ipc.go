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

	"agentmux/internal/pathutil"
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
	dir := filepath.Join(cache, "agentmux", hash)
	address := filepath.Join(dir, "broker.sock")
	if runtime.GOOS == "windows" {
		address = `\\.\pipe\agentmux-` + hash
	} else if len(address) > 100 {
		// sockaddr_un is limited to roughly 104 bytes on macOS.
		dir = filepath.Join(os.TempDir(), "agentmux-"+hash)
		address = filepath.Join(dir, "broker.sock")
	}
	return Endpoint{Directory: dir, Address: address}, nil
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
