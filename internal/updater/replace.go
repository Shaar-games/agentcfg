package updater

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gofrs/flock"
)

// replaceExecutable stages and validates before touching the existing binary.
// A lock serializes concurrent 'agentmux update' invocations. Installer
// scripts have their own replacement/rollback logic.
func replaceExecutable(ctx context.Context, target, stage, version string, expectedDigest [sha256.Size]byte) (string, error) {
	lock := flock.New(target + ".update.lock")
	defer lock.Close()
	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	locked, err := lock.TryLockContext(lockCtx, 50*time.Millisecond)
	if err != nil {
		return "", fmt.Errorf("acquire update lock: %w", err)
	}
	if !locked {
		return "", errors.New("another self-update is already running")
	}
	defer lock.Unlock()

	info, err := os.Lstat(target)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular installed executable: %s", target)
	}
	currentDigest, err := executableHash(target)
	if err != nil {
		return "", err
	}
	if currentDigest != expectedDigest {
		return "", errors.New("executable was changed by another installer during update; original files preserved")
	}
	backup, err := reserveBackup(filepath.Dir(target), filepath.Ext(target))
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		// A mapped Windows image cannot be replaced in place. Rename the
		// running executable first, so there is always a rollback candidate.
		if err := os.Rename(target, backup); err != nil {
			return "", fmt.Errorf("rename active executable for update: %w", err)
		}
		if err := os.Rename(stage, target); err != nil {
			if rollbackErr := os.Rename(backup, target); rollbackErr != nil {
				return backup, fmt.Errorf("install new executable: %w; rollback from %s also failed: %v", err, backup, rollbackErr)
			}
			return "", fmt.Errorf("install new executable (restored original): %w", err)
		}
	} else {
		if err := copyBackup(target, backup, info.Mode().Perm()); err != nil {
			_ = os.Remove(backup)
			return "", fmt.Errorf("back up executable: %w", err)
		}
		// POSIX rename swaps the executable atomically while a running
		// process continues using its old mapped file.
		if err := os.Rename(stage, target); err != nil {
			_ = os.Remove(backup)
			return "", fmt.Errorf("install new executable (original retained): %w", err)
		}
	}
	if err := verifyVersion(ctx, target, version); err != nil {
		// Restore the earlier verified executable on any post-install error.
		if runtime.GOOS == "windows" {
			if removeErr := os.Remove(target); removeErr != nil {
				return backup, fmt.Errorf("installed binary validation failed: %v; cannot remove failed binary; original retained at %s: %w", err, backup, removeErr)
			}
		}
		if rollbackErr := os.Rename(backup, target); rollbackErr != nil {
			return backup, fmt.Errorf("installed binary validation failed: %v; restore backup from %s failed: %w", err, backup, rollbackErr)
		}
		backup = ""
		return "", fmt.Errorf("installed binary validation failed; previous binary restored: %w", err)
	}
	// Removing a running Windows executable's backup may be denied by the OS.
	// Leave it intact for manual cleanup after this process exits.
	if err := os.Remove(backup); err != nil {
		// A process still executing the previous Windows image keeps its
		// mapped file alive. Never remove that rollback copy on failure.
		return backup, nil
	}
	return "", nil
}

func reserveBackup(dir, extension string) (string, error) {
	file, err := os.CreateTemp(dir, ".agentmux-backup-*"+extension)
	if err != nil {
		return "", fmt.Errorf("reserve backup path: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return "", err
	}
	// Rename on Windows does not replace an existing destination.
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func copyBackup(source, destination string, mode os.FileMode) error {
	from, err := os.Open(source)
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copiedErr := io.Copy(to, from)
	if copiedErr == nil {
		copiedErr = to.Sync()
	}
	closeErr := to.Close()
	if copiedErr != nil {
		return copiedErr
	}
	if err := os.Chmod(destination, mode); err != nil {
		return err
	}
	return closeErr
}

func executableHash(path string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return digest, err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return digest, err
	}
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}
