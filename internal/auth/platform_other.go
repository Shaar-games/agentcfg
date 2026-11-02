//go:build !windows

package auth

import (
	"fmt"
	"os"
)

func systemProtector() (protector, error) { return nil, ErrSecureStoreUnavailable }
func privatePath(path string, directory bool) error {
	mode := os.FileMode(0600)
	if directory {
		mode = 0700
	}
	return os.Chmod(path, mode)
}
func checkPrivatePath(_ string, info os.FileInfo) error {
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("OAuth credential paths must be accessible only by the current user")
	}
	return nil
}
