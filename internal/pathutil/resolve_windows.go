//go:build windows

package pathutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Resolve returns the absolute final filesystem path of an existing entry.
// A handle opened without FILE_FLAG_OPEN_REPARSE_POINT follows both symbolic
// links and mount-point junctions. EvalSymlinks alone does not resolve every
// junction on supported Windows toolchains.
func Resolve(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(absolute, "\\\\.\\") {
		return "", fmt.Errorf("device namespace is not a package filesystem path")
	}
	name, err := windows.UTF16PtrFromString(extendedWindowsPath(absolute))
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", &os.PathError{Op: "resolve", Path: path, Err: err}
	}
	defer windows.CloseHandle(handle)
	for size := uint32(512); size <= 32768; {
		buffer := make([]uint16, size)
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], size, 0)
		if err != nil {
			return "", &os.PathError{Op: "resolve", Path: path, Err: err}
		}
		if length < size {
			return filepath.Clean(normalWindowsPath(windows.UTF16ToString(buffer[:length]))), nil
		}
		size = length + 1
	}
	return "", fmt.Errorf("resolved Windows path exceeds 32768 UTF-16 code units")
}

func extendedWindowsPath(path string) string {
	if strings.HasPrefix(path, "\\\\?\\") {
		return path
	}
	if strings.HasPrefix(path, "\\\\") {
		return "\\\\?\\UNC\\" + path[2:]
	}
	return "\\\\?\\" + path
}

func normalWindowsPath(path string) string {
	if strings.HasPrefix(path, "\\\\?\\UNC\\") {
		return "\\\\" + path[8:]
	}
	if strings.HasPrefix(path, "\\\\?\\") && len(path) > 5 && path[5] == ':' {
		return path[4:]
	}
	return path
}
