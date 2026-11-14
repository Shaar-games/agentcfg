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
		// sockaddr_un is limited to roughly 104 bytes on macOS.
		// A user-specific TMPDIR may also be too long. Retain the same UID/root
		// identity in the short system temporary directory; Prepare enforces
		// ownership, permissions, and the refusal of a symlink at this path.
		for _, temporary := range []string{os.TempDir(), "/tmp"} {
			dir = filepath.Join(temporary, "agentcfg-"+hash)
			address = filepath.Join(dir, "broker.sock")
			if len(address) <= 100 {
				break
			}
		}
		if len(address) > 100 {
			return Endpoint{}, fmt.Errorf("Unix IPC address exceeds the 100-byte limit")
		}
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
