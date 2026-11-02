//go:build !windows

package ipc

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
)

func secureDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("runtime directory is not owned by this user")
	}
	return os.Chmod(path, 0o700)
}

// Listen must be called under the broker's lifetime lock. Only stale socket
// files (never arbitrary files or symlinks) are removed.
func Listen(endpoint Endpoint) (net.Listener, error) {
	if info, err := os.Lstat(endpoint.Address); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket %s", endpoint.Address)
		}
		if err := os.Remove(endpoint.Address); err != nil {
			return nil, err
		}
	}
	listener, err := net.Listen("unix", endpoint.Address)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(endpoint.Address, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func Dial(ctx context.Context, endpoint Endpoint) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", endpoint.Address)
}

func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func RetryInInheritedJob(_ *exec.Cmd, _ error) bool { return false }
