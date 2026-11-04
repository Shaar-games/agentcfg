package syncer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentcfg/internal/pathutil"
)

// normalizeScopeRoot binds an explicitly selected root to its physical identity,
// including a linked user profile, macOS /var or an NTFS short name.
// Descendants are deliberately not resolved here: doing so could erase a
// symlink or junction that the write-path checks need to reject.
func normalizeScopeRoot(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	before, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !before.IsDir() || !os.SameFile(before, before) {
		return "", fmt.Errorf("scope root must resolve to a directory: %s", path)
	}
	physical, err := pathutil.Resolve(absolute)
	if err != nil {
		return "", err
	}
	for _, candidate := range []string{absolute, physical} {
		after, err := os.Stat(candidate)
		if err != nil || !after.IsDir() || !os.SameFile(before, after) {
			return "", fmt.Errorf("scope root changed while resolving aliases: %s", path)
		}
	}
	return physical, nil
}

func withinScope(relative string) bool {
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

// scopePath rebases only the root spelling. It never resolves a descendant.
// When lexical containment does not hold, the outermost real ancestor with the
// root's identity supplies the relative suffix. Choosing the innermost match
// could hide an in-home link that points back to an ancestor of the home.
func scopePath(base, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("target escapes selected scope: %s", path)
	}
	root, err := normalizeScopeRoot(base)
	if err != nil {
		return "", err
	}
	path = filepath.Clean(path)
	if relative, err := filepath.Rel(root, path); err == nil && withinScope(relative) {
		return filepath.Join(root, relative), nil
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !os.SameFile(rootInfo, rootInfo) {
		return "", fmt.Errorf("cannot capture scope root identity: %s", root)
	}
	ancestors := []string{}
	for candidate := path; ; candidate = filepath.Dir(candidate) {
		ancestors = append(ancestors, candidate)
		if filepath.Dir(candidate) == candidate {
			break
		}
	}
	outermost := ""
	// Inspect from the filesystem root towards the leaf. Once the approved
	// home is identified, do not stat any unvalidated descendants through it.
	for i := len(ancestors) - 1; i >= 0; i-- {
		candidate := ancestors[i]
		info, err := os.Lstat(candidate)
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		if err == nil && info.IsDir() && !redirected(info) && os.SameFile(rootInfo, info) {
			outermost = candidate
			break
		}
	}
	if outermost == "" {
		return "", fmt.Errorf("target escapes selected scope: %s", path)
	}
	relative, err := filepath.Rel(outermost, path)
	if err != nil || !withinScope(relative) {
		return "", fmt.Errorf("target escapes selected scope: %s", path)
	}
	return filepath.Join(root, relative), nil
}

// Ownership hashes remain authoritative across different spellings of the same
// root. Only map keys are rebased; the manifest's original bytes remain in
// Plan.stateBefore for CAS. Two records for one destination are ambiguous even
// when their hashes happen to match.
func normalizeManifestPaths(old manifest, base string) (manifest, error) {
	var err error
	if old.Configs, err = normalizeManifestMap(base, "config", old.Configs); err != nil {
		return old, err
	}
	if old.Blocks, err = normalizeManifestMap(base, "TOML block", old.Blocks); err != nil {
		return old, err
	}
	if old.Skills, err = normalizeManifestMap(base, "skill", old.Skills); err != nil {
		return old, err
	}
	if old.Instructions, err = normalizeManifestMap(base, "instructions", old.Instructions); err != nil {
		return old, err
	}
	return old, nil
}

func normalizeManifestMap[T any](base, kind string, entries map[string]T) (map[string]T, error) {
	result := make(map[string]T, len(entries))
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		normalized, err := scopePath(base, path)
		if err != nil {
			return nil, fmt.Errorf("invalid ownership %s path: %w", kind, err)
		}
		if _, duplicate := result[normalized]; duplicate {
			return nil, fmt.Errorf("ownership manifest has duplicate aliases for %s path %s", kind, normalized)
		}
		result[normalized] = entries[path]
	}
	return result, nil
}
