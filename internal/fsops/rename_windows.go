//go:build windows

package fsops

import (
	"errors"
	"os"
	"path/filepath"

	"agentcfg/internal/pathutil"
	"golang.org/x/sys/windows"
)

func retryRename(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

func renameOnce(source, destination string, replace bool) error {
	if replace {
		return os.Rename(source, destination)
	}
	old, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	newPath, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	oldName, err := windows.UTF16PtrFromString(pathutil.WindowsAPIPath(old))
	if err != nil {
		return err
	}
	newName, err := windows.UTF16PtrFromString(pathutil.WindowsAPIPath(newPath))
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(oldName, newName, 0); err != nil {
		return &os.LinkError{Op: "rename", Old: source, New: destination, Err: err}
	}
	return nil
}
