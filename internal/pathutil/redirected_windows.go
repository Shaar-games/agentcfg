//go:build windows

package pathutil

import (
	"os"
	"syscall"
)

// IsRedirected handles Windows junctions as well as ordinary symlinks.
// Go's ModeSymlink is not sufficient for every NTFS reparse point.
func IsRedirected(info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && attributes.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
