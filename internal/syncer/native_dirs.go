package syncer

import (
	"fmt"
	"os"
	"path/filepath"
)

// Track directories created by this transaction so rollback restores the native
// layout too. Never remove a directory another process created or replaced.
func prepareNativeParent(base, path string) (func() error, error) {
	type createdDirectory struct {
		path     string
		identity os.FileInfo
	}
	var created []createdDirectory
	cleanup := func() error {
		for i := len(created) - 1; i >= 0; i-- {
			dir := created[i]
			if err := ensureNoSymlinkParents(base, dir.path); err != nil {
				return err
			}
			info, err := os.Lstat(dir.path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil || !os.SameFile(dir.identity, info) {
				return fmt.Errorf("native parent directory changed during rollback: %s", dir.path)
			}
			children, err := os.ReadDir(dir.path)
			if err != nil {
				return err
			}
			if len(children) == 0 {
				if err := os.Remove(dir.path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	var missing []string
	for parent := filepath.Dir(path); !sameAdoptionPath(parent, base); parent = filepath.Dir(parent) {
		if !pathUnder(base, parent) {
			return cleanup, fmt.Errorf("native parent escapes target home: %s", parent)
		}
		if _, err := os.Lstat(parent); os.IsNotExist(err) {
			missing = append(missing, parent)
		} else if err != nil {
			return cleanup, err
		} else {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		dir := missing[i]
		if err := ensureNoSymlinkParents(base, dir); err != nil {
			return cleanup, err
		}
		if err := os.Mkdir(dir, 0700); os.IsExist(err) {
			continue
		} else if err != nil {
			return cleanup, err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return cleanup, err
		}
		if !os.SameFile(info, info) {
			return cleanup, fmt.Errorf("cannot capture native parent identity: %s", dir)
		}
		created = append(created, createdDirectory{dir, info})
	}
	if len(created) == 0 {
		return nil, nil
	}
	return cleanup, nil
}
