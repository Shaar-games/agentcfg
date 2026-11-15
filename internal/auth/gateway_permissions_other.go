//go:build !windows

package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"agentcfg/internal/pathutil"
)

// CheckGatewayConfigPrivacy verifies, without changing permissions, that a
// native config can contain a gateway URL capability. New files must be created
// with mode 0600 and new directories with mode 0700 by the caller.
func CheckGatewayConfigPrivacy(path string) error {
	return checkGatewayConfigPrivacy(path, nil)
}

// CheckGatewayConfigCopyPrivacy also verifies the exact mode of the future
// copied file. A copy may retain 0644 even when its source has a private parent.
func CheckGatewayConfigCopyPrivacy(path string, mode os.FileMode) error {
	return checkGatewayConfigPrivacy(path, &mode)
}

// CheckGatewayConfigMovePrivacy checks an existing file at its future location
// after moving sourceRoot to destinationRoot, retaining the moved tree's modes.
func CheckGatewayConfigMovePrivacy(sourceRoot, destinationRoot, relative string) error {
	file, paths, _, err := gatewayMovePaths(sourceRoot, destinationRoot, relative)
	if err != nil {
		return err
	}
	if err := CheckGatewayConfigPrivacy(file); err != nil {
		return err
	}
	info, err := os.Lstat(file)
	if err != nil {
		return err
	}
	var parents []os.FileInfo
	for _, path := range paths {
		parent, err := os.Lstat(path)
		if err != nil {
			return err
		}
		parents = append(parents, parent)
	}
	return checkGatewayUnixPrivacy(info, parents, uint32(os.Geteuid()))
}

func checkGatewayConfigPrivacy(path string, copyMode *os.FileMode) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve gateway configuration permissions: %w", err)
	}
	file, err := os.Lstat(absolute)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect gateway configuration permissions: %w", err)
	}
	if file != nil && (pathutil.IsRedirected(file) || !file.Mode().IsRegular()) {
		return errors.New("native OAuth gateway configuration must be a regular file, not a redirected path")
	}
	var parents []os.FileInfo
	for parent := filepath.Dir(absolute); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect gateway configuration parent permissions: %w", err)
		}
		if info != nil {
			if !info.IsDir() || pathutil.IsRedirected(info) {
				return errors.New("native OAuth gateway configuration parent must be a directory, not a redirected path")
			}
			parents = append(parents, info)
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	uid := uint32(os.Geteuid())
	if copyMode != nil {
		return checkGatewayUnixCopyPrivacy(file, parents, uid, *copyMode)
	}
	return checkGatewayUnixPrivacy(file, parents, uid)
}

func checkGatewayUnixCopyPrivacy(file os.FileInfo, parents []os.FileInfo, uid uint32, mode os.FileMode) error {
	if err := checkGatewayUnixPrivacy(file, parents, uid); err != nil {
		return err
	}
	if mode.Perm()&0077 == 0 {
		return nil
	}
	for _, parent := range parents {
		if parent.Mode().Perm()&0011 == 0 {
			return nil
		}
	}
	return errors.New("native OAuth gateway copy would be accessible to another account; restrict parent traversal before copying")
}

// Parents are ordered from the nearest existing parent to the filesystem root.
func checkGatewayUnixPrivacy(file os.FileInfo, parents []os.FileInfo, uid uint32) error {
	owned := func(info os.FileInfo) bool {
		stat, ok := info.Sys().(*syscall.Stat_t)
		return ok && (stat.Uid == uid || stat.Uid == 0)
	}
	if file != nil && !owned(file) {
		return errors.New("native OAuth gateway configuration is owned by another account")
	}
	if len(parents) == 0 {
		return errors.New("native OAuth gateway configuration parent permissions cannot be verified")
	}
	protected := false
	for index := len(parents) - 1; index >= 0; index-- {
		parent := parents[index]
		if !owned(parent) {
			return errors.New("native OAuth gateway configuration parent is owned by another account")
		}
		// A sticky system temporary directory may contain an already-owned
		// private directory. A shared immediate destination is not accepted.
		stickyAncestor := index > 0 && parent.Mode()&os.ModeSticky != 0
		if !protected && parent.Mode().Perm()&0022 != 0 && !stickyAncestor {
			return errors.New("native OAuth gateway configuration parent is writable by another account; restrict its permissions before syncing")
		}
		if parent.Mode().Perm()&0011 == 0 {
			protected = true
		}
	}
	if file == nil || file.Mode().Perm()&0077 == 0 || protected {
		return nil
	}
	return errors.New("native OAuth gateway configuration is accessible to another account; restrict file permissions or parent traversal before syncing")
}
