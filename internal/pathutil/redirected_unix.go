//go:build !windows

package pathutil

import "os"

// IsRedirected identifies a filesystem entry which must not be followed when
// creating a user-owned configuration directory.
func IsRedirected(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0
}
