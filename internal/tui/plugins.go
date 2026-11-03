package tui

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"agentcfg/internal/config"
)

const pluginSchema = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"

type pluginInfo struct {
	Name, Path, Description, Problem string
	MCP, Skills                      int
	Valid                            bool
}

// discoverPlugins only inventories portable manifests from the user's source
// root. Config.Load remains the authority for whether a component is usable.
func discoverPlugins(root string, cfg *config.Config) []pluginInfo {
	base := filepath.Join(root, ".agentcfg", "plugins")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	plugins := make([]pluginInfo, 0, len(entries))
	for _, entry := range entries {
		path := filepath.Join(base, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		item := pluginInfo{Name: entry.Name(), Path: path}
		// A linked plugin may be supported when its contents remain inside the
		// plugin boundary. Never inspect arbitrary destinations outside it.
		resolvedBase, errBase := filepath.EvalSymlinks(base)
		resolvedPath, errPath := filepath.EvalSymlinks(path)
		if errBase != nil || errPath != nil || !isWithin(resolvedBase, resolvedPath) {
			item.Problem = "Plugin path leaves the canonical plugins directory"
			plugins = append(plugins, item)
			continue
		}
		manifestPath := filepath.Join(path, "plugin.json")
		manifestInfo, err := os.Lstat(manifestPath)
		if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Size() > 1<<20 {
			item.Problem = "Missing or invalid plugin.json"
			plugins = append(plugins, item)
			continue
		}
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			item.Problem = "Cannot read plugin.json"
			plugins = append(plugins, item)
			continue
		}
		var document struct {
			Schema      string `json:"$schema"`
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if json.Unmarshal(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}), &document) != nil || document.Schema != pluginSchema || document.Name == "" {
			item.Problem = "Unsupported or invalid plugin manifest"
			plugins = append(plugins, item)
			continue
		}
		item.Name, item.Description = document.Name, document.Description
		prefix := item.Name + "."
		for name := range cfg.Servers {
			if strings.HasPrefix(name, prefix) {
				item.MCP++
			}
		}
		for _, skill := range cfg.Skills {
			if strings.HasPrefix(skill.Name, prefix) {
				item.Skills++
			}
		}
		for _, warning := range cfg.Warnings {
			if strings.Contains(warning, path+string(filepath.Separator)) || strings.Contains(warning, path+":") {
				item.Problem = "One or more components failed validation"
				break
			}
		}
		item.Valid = item.Problem == ""
		plugins = append(plugins, item)
	}
	return plugins
}

func isWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
