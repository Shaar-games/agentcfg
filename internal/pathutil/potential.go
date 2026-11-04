package pathutil

import (
	"os"
	"path/filepath"
)

// ResolvePotential resolves existing ancestors without treating a dangling link
// as a missing directory. It creates nothing and preserves a missing tail.
func ResolvePotential(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err = os.Lstat(absolute); err == nil {
		return Resolve(absolute)
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(absolute)
	if parent == absolute {
		return "", err
	}
	resolved, err := ResolvePotential(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(absolute)), nil
}
