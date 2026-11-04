//go:build !windows

package fsops

import "os"

func renameOnce(source, destination string, replace bool) error {
	return os.Rename(source, destination)
}
func retryRename(error) bool { return false }
