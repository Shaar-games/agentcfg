// Package fsops provides checked filesystem mutations shared by init and sync.
package fsops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const renameWait = 5 * time.Second

// Rename moves an entry to a new name. A destination that appears while waiting
// is never adopted as an overwrite permission. Check is rerun before each try.
func Rename(ctx context.Context, source, destination string, check func() error) error {
	return rename(ctx, source, destination, false, check, renameWait)
}

// Replace is for atomic file replacement with the caller's preimage check.
func Replace(ctx context.Context, source, destination string, check func() error) error {
	return rename(ctx, source, destination, true, check, renameWait)
}

type renameEntry struct {
	path     string
	info     os.FileInfo
	contents bool
}

func rename(ctx context.Context, source, destination string, replace bool, check func() error, wait time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var pinned []renameEntry
	for i, path := range []string{source, destination, filepath.Dir(source), filepath.Dir(destination)} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && i == 1 {
			pinned = append(pinned, renameEntry{path: path})
			continue
		}
		if err != nil {
			return err
		}
		if i == 1 && !replace {
			return fmt.Errorf("rename destination already exists: %s", destination)
		}
		if !os.SameFile(info, info) {
			return fmt.Errorf("cannot capture rename identity: %s", path)
		}
		pinned = append(pinned, renameEntry{path, info, i < 2})
	}
	started := time.Now()
	delay := 20 * time.Millisecond
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(last, err)
		}
		for _, entry := range pinned {
			current, err := os.Lstat(entry.path)
			if entry.info == nil && os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if entry.info == nil || !os.SameFile(entry.info, current) || entry.info.Mode() != current.Mode() || entry.contents && (!entry.info.ModTime().Equal(current.ModTime()) || !current.IsDir() && entry.info.Size() != current.Size()) {
				return fmt.Errorf("filesystem entry changed while waiting to rename: %s", entry.path)
			}
		}
		var err error
		if check != nil {
			err = check()
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = renameOnce(source, destination, replace)
		}
		if err == nil {
			return nil
		}
		last = err
		if !retryRename(err) {
			return err
		}
		remaining := wait - time.Since(started)
		if remaining <= 0 {
			return fmt.Errorf("%w (rename still blocked after %s; original data retained)", err, wait)
		}
		if delay > remaining {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
		if delay < 200*time.Millisecond {
			delay *= 2
			if delay > 200*time.Millisecond {
				delay = 200 * time.Millisecond
			}
		}
	}
}
