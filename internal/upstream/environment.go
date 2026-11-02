package upstream

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

func environmentKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

// mergeEnvironment sorts overrides so even Windows case aliases are resolved
// deterministically. The lexically last spelling wins when aliases coexist.
func mergeEnvironment(parent []string, overrides map[string]string) ([]string, error) {
	entries := make(map[string]string, len(parent)+len(overrides))
	for _, entry := range parent {
		// Windows may include drive working directories named "=C:".
		separator := strings.IndexByte(entry[minimum(1, len(entry)):], '=')
		if separator < 0 {
			continue
		}
		separator += minimum(1, len(entry))
		entries[environmentKey(entry[:separator])] = entry
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := overrides[key]
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return nil, errors.New("invalid environment variable name or value")
		}
		entries[environmentKey(key)] = key + "=" + value
	}
	keys = keys[:0]
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, entries[key])
	}
	return result, nil
}

func minimum(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func environmentValue(env []string, key string) string {
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && environmentKey(name) == environmentKey(key) {
			return value
		}
	}
	return ""
}

func workingDirectory(cwd string) (string, error) {
	if cwd == "" {
		cwd = "."
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve MCP working directory: %w", err)
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return "", errors.New("MCP working directory does not exist or is not a directory")
	}
	return cwd, nil
}

func resolveCommand(command, cwd string, env []string) (string, error) {
	if command == "" || strings.ContainsRune(command, '\x00') {
		return "", errors.New("MCP command is empty or invalid")
	}
	extensions := []string{""}
	if runtime.GOOS == "windows" && filepath.Ext(command) == "" {
		pathext := environmentValue(env, "PATHEXT")
		if pathext == "" {
			pathext = ".COM;.EXE;.BAT;.CMD"
		}
		for _, ext := range strings.Split(pathext, ";") {
			if strings.HasPrefix(ext, ".") && !strings.ContainsAny(ext, "/\\:") {
				extensions = append(extensions, ext)
			}
		}
	}
	var candidates []string
	if strings.ContainsAny(command, "/\\") || filepath.VolumeName(command) != "" {
		if !filepath.IsAbs(command) {
			command = filepath.Join(cwd, command)
		}
		candidates = append(candidates, command)
	} else {
		for _, directory := range filepath.SplitList(environmentValue(env, "PATH")) {
			if directory == "" {
				directory = "."
			}
			if !filepath.IsAbs(directory) {
				directory = filepath.Join(cwd, directory)
			}
			candidates = append(candidates, filepath.Join(directory, command))
		}
	}
	for _, candidate := range candidates {
		for _, ext := range extensions {
			path := candidate + ext
			info, err := os.Stat(path)
			if err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0111 != 0) {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("MCP executable %q was not found in the configured working directory/PATH", command)
}
