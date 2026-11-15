package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"agentcfg/internal/pathutil"
)

// gatewayMovePaths describes the permissions after a rename. Source ancestors
// above the moved root disappear from that chain; the replaced destination
// itself may still be a link and must not be followed to infer future privacy.
func gatewayMovePaths(sourceRoot, destinationRoot, relative string) (string, []string, int, error) {
	sourceRoot, err := filepath.Abs(sourceRoot)
	if err != nil {
		return "", nil, 0, err
	}
	destinationRoot, err = filepath.Abs(destinationRoot)
	if err != nil {
		return "", nil, 0, err
	}
	relative = filepath.Clean(filepath.FromSlash(relative))
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", nil, 0, errors.New("invalid gateway configuration path inside moved layout")
	}
	file := filepath.Join(sourceRoot, relative)
	var parents []string
	if relative != "." {
		for parent := filepath.Dir(file); ; parent = filepath.Dir(parent) {
			parents = append(parents, parent)
			if parent == sourceRoot {
				break
			}
		}
	}
	movedParents := len(parents)
	for parent := filepath.Dir(destinationRoot); ; parent = filepath.Dir(parent) {
		parents = append(parents, parent)
		if filepath.Dir(parent) == parent {
			break
		}
	}
	for _, parent := range parents {
		info, err := os.Lstat(parent)
		if err != nil {
			return "", nil, 0, err
		}
		if !info.IsDir() || pathutil.IsRedirected(info) {
			return "", nil, 0, errors.New("native OAuth gateway move requires physical source and destination parents")
		}
	}
	return file, parents, movedParents, nil
}
