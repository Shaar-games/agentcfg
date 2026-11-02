package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentmux/internal/pathutil"
)

func resolveExisting(path string) (string, error) {
	return pathutil.Resolve(path)
}

func requireDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: expected a directory", path)
	}
	return nil
}

func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

// resolvePotential resolves every existing ancestor, including symlinks and
// Windows junctions. Missing tails are allowed without creating directories.
// An existing dangling link is an error, not a missing lexical directory.
func resolvePotential(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err = os.Lstat(absolute); err == nil {
		return resolveExisting(absolute)
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(absolute)
	if parent == absolute {
		return "", err
	}
	resolved, err := resolvePotential(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(absolute)), nil
}

func containedPath(root, path string, missing bool) (string, error) {
	resolvedRoot, err := resolvePotential(root)
	if err != nil {
		return "", err
	}
	var resolved string
	if missing {
		resolved, err = resolvePotential(path)
	} else {
		resolved, err = resolveExisting(path)
	}
	if err != nil {
		return "", err
	}
	if !within(resolvedRoot, resolved) {
		return "", fmt.Errorf("path escapes permitted root %s", resolvedRoot)
	}
	return resolved, nil
}

func pluginRelative(root, value string, missing bool) (string, error) {
	if !strings.HasPrefix(value, "./") {
		return "", fmt.Errorf("plugin-relative path must begin with ./")
	}
	if strings.ContainsAny(value, "\\:\x00") {
		return "", fmt.Errorf("plugin-relative path must use portable forward-slash components")
	}
	return containedPath(root, filepath.Join(root, filepath.FromSlash(value)), missing)
}

func pluginDataRoot(sourceRoot, name string) (string, error) {
	base := filepath.Join(sourceRoot, ".agentmux", "data", "plugins")
	candidate := filepath.Join(base, name)
	resolved, err := containedPath(sourceRoot, candidate, true)
	if err != nil {
		return "", fmt.Errorf("PLUGIN_DATA: %w", err)
	}
	// A link into another in-project directory would share unrelated data.
	resolvedBase, err := resolvePotential(base)
	if err != nil {
		return "", err
	}
	expected := filepath.Join(resolvedBase, name)
	if !within(expected, resolved) || !within(resolved, expected) {
		return "", fmt.Errorf("PLUGIN_DATA must be dedicated to this plugin")
	}
	if info, statErr := os.Stat(resolved); statErr == nil && !info.IsDir() {
		return "", fmt.Errorf("PLUGIN_DATA must be a directory")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return "", statErr
	}
	return resolved, nil
}

func expandPlugin(value, root, data string) string {
	return strings.NewReplacer("${PLUGIN_ROOT}", root, "${PLUGIN_DATA}", data).Replace(value)
}

func pluginWorkingDirectory(value, root, data string) (string, error) {
	if value == "" {
		return root, nil
	}
	if strings.HasPrefix(value, "./") {
		if strings.ContainsAny(value, "\\:\x00") {
			return "", fmt.Errorf("cwd must use portable forward-slash components")
		}
		expanded := expandPlugin(value, root, data)
		return containedPath(root, filepath.Join(root, filepath.FromSlash(expanded)), false)
	}
	for _, candidate := range []struct{ token, base string }{
		{"${PLUGIN_ROOT}", root}, {"${PLUGIN_DATA}", data},
	} {
		if value != candidate.token && !strings.HasPrefix(value, candidate.token+"/") {
			continue
		}
		tail := strings.TrimPrefix(value, candidate.token)
		if strings.ContainsAny(tail, "\\:\x00") {
			return "", fmt.Errorf("cwd must use portable forward-slash components")
		}
		return containedPath(candidate.base, expandPlugin(value, root, data), candidate.token == "${PLUGIN_DATA}")
	}
	return "", fmt.Errorf("cwd must start with ./, ${PLUGIN_ROOT}, or ${PLUGIN_DATA}")
}
