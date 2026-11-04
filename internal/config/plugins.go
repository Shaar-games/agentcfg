package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (l *loader) loadPlugins() {
	path := filepath.Join(l.agents, "plugins")
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		l.warn(path, err)
		return
	}
	names := map[string]string{}
	for _, entry := range entries {
		candidate := filepath.Join(path, entry.Name())
		root, err := resolveExisting(candidate)
		if err != nil {
			l.warn(candidate, err)
			continue
		}
		info, err := os.Stat(root)
		if err != nil {
			l.warn(candidate, err)
			continue
		}
		if !info.IsDir() {
			continue
		}
		name, err := l.loadManifest(root)
		if err != nil {
			l.warn(candidate, err)
			continue
		}
		if previous, exists := names[name]; exists {
			l.warn(candidate, fmt.Errorf("duplicate plugin name %q; keeping %s (directory order)", name, previous))
			continue
		}
		names[name] = candidate
		l.pluginRoots = append(l.pluginRoots, root)
		l.loadPluginServers(root, name)
		l.loadSkills(filepath.Join(candidate, "skills"), name, candidate, false)
	}
}

func (l *loader) loadManifest(root string) (string, error) {
	path, err := linkedReadPath(root, filepath.Join(root, "plugin.json"), false)
	if err != nil {
		return "", fmt.Errorf("plugin.json: %w", err)
	}
	data, err := readBounded(path)
	if err != nil {
		return "", fmt.Errorf("plugin.json: %w", err)
	}
	fields, err := object(data)
	if err != nil {
		return "", fmt.Errorf("plugin.json: %w", err)
	}
	schema, err := stringField(fields, "$schema", true)
	if err != nil {
		return "", err
	}
	if schema != pluginSchema {
		return "", fmt.Errorf("unsupported plugin $schema %q; expected %s", schema, pluginSchema)
	}
	name, err := stringField(fields, "name", true)
	if err != nil {
		return "", err
	}
	if !validPluginName(name) {
		return "", fmt.Errorf("plugin name must be 1-64 lowercase ASCII letters, numbers, dots or hyphens; alphanumeric ends; no -- or ..")
	}
	if err = validateManifestMetadata(fields); err != nil {
		return "", err
	}
	known := map[string]bool{"$schema": true, "name": true, "version": true, "description": true,
		"author": true, "homepage": true, "repository": true, "license": true, "keywords": true, "extensions": true}
	for _, key := range sortedKeys(fields) {
		if !known[key] {
			l.warn(path, fmt.Errorf("unknown manifest field %q ignored; use extensions for client-owned data", key))
		}
	}
	if raw, exists := fields["extensions"]; exists {
		var extensions map[string]json.RawMessage
		if json.Unmarshal(raw, &extensions) != nil || extensions == nil {
			l.warn(path, fmt.Errorf("non-object extensions ignored"))
		}
		// No extension namespace is implemented. Its values are deliberately
		// not validated and cannot replace portable component locations.
	}
	return name, nil
}

func validPluginName(name string) bool {
	if len(name) < 1 || len(name) > 64 || strings.Contains(name, "--") || strings.Contains(name, "..") {
		return false
	}
	alnum := func(char byte) bool { return char >= 'a' && char <= 'z' || char >= '0' && char <= '9' }
	if !alnum(name[0]) || !alnum(name[len(name)-1]) {
		return false
	}
	for i := range len(name) {
		if !alnum(name[i]) && name[i] != '-' && name[i] != '.' {
			return false
		}
	}
	return true
}

func validateManifestMetadata(fields map[string]json.RawMessage) error {
	for _, key := range []string{"version", "description", "homepage", "repository", "license"} {
		if _, err := stringField(fields, key, false); err != nil {
			return err
		}
	}
	if _, err := stringList(fields["keywords"], "keywords"); err != nil {
		return err
	}
	if raw, exists := fields["author"]; exists {
		author, err := object(raw)
		if err != nil {
			return fmt.Errorf("author: %w", err)
		}
		if err = allowFields(author, "name", "email", "url"); err != nil {
			return fmt.Errorf("author: %w", err)
		}
		for _, key := range sortedKeys(author) {
			if _, err = stringField(author, key, true); err != nil {
				return fmt.Errorf("author: %w", err)
			}
		}
	}
	return nil
}

func (l *loader) loadPluginServers(root, name string) {
	location := filepath.Join(root, "mcp.json")
	if _, err := os.Lstat(location); os.IsNotExist(err) {
		return
	}
	path, err := linkedReadPath(root, location, false)
	if err != nil {
		l.warn(location, err)
		return
	}
	data, err := readBounded(path)
	if err != nil {
		l.warn(location, err)
		return
	}
	entries, err := serverDocument(data, true)
	if err != nil {
		l.warn(location, err)
		return
	}
	l.loadServerEntries(entries, location, root, name)
}
