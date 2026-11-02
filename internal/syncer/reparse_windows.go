//go:build windows

package syncer

import (
	"os"
	"syscall"
)

// Junctions and other reparse points are not always exposed as ModeSymlink.
// Never traverse one when writing a generated destination or copying a tree.
func redirected(info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && attributes.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
