//go:build !windows

package pathutil

import "path/filepath"

// Resolve returns the absolute final filesystem path of an existing entry.
func Resolve(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}
