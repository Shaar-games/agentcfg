package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const (
	pluginSchema   = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	mcpSchema      = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
	originMarker   = ".agentcfg-origin.json"
	maxConfigBytes = 1 << 20
)

type loader struct {
	config      *Config
	agents      string
	pluginRoots []string
}

// Load resolves source configuration without starting processes, fetching
// schemas, creating data directories, or writing harness configuration.
// Invalid settings/source documents return errors; invalid entries and plugin
// components are skipped independently with deterministic warnings.
func Load(root string) (*Config, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	root, err = resolveExisting(root)
	if err != nil {
		return nil, fmt.Errorf("source root: %w", err)
	}
	if err = requireDirectory(root); err != nil {
		return nil, err
	}
	l := &loader{config: &Config{
		Root: root, Servers: map[string]Server{}, Skills: []Skill{}, Warnings: []string{},
		Settings: defaultSettings(),
	}, agents: filepath.Join(root, ".agentcfg")}
	if err = l.loadSettings(); err != nil {
		return nil, err
	}
	if err = l.loadCanonicalServers(); err != nil {
		return nil, err
	}
	l.loadPlugins()
	l.loadSkills(filepath.Join(l.agents, "skills"), "", l.agents, true)
	l.applyOverrides()
	sort.Slice(l.config.Skills, func(i, j int) bool {
		return l.config.Skills[i].Name < l.config.Skills[j].Name
	})
	return l.config, nil
}

func (l *loader) warn(source string, err error) {
	l.config.Warnings = append(l.config.Warnings, source+": "+err.Error())
}

func (l *loader) loadCanonicalServers() error {
	path := filepath.Join(l.agents, "mcp.json")
	data, err := readBounded(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	entries, err := serverDocument(data, false)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	l.loadServerEntries(entries, path, "", "")
	return nil
}

func (l *loader) addServer(server Server) {
	if previous, exists := l.config.Servers[server.Name]; exists {
		l.warn(server.Source, fmt.Errorf("server %q collides with %s; keeping the first entry (canonical sources take precedence)", server.Name, previous.Source))
		return
	}
	l.config.Servers[server.Name] = server
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
