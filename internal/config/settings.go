package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func defaultSettings() Settings {
	return Settings{
		Harnesses:       []string{"claude-code", "codex", "cursor", "opencode", "gemini"},
		Runtime:         Runtime{IdleTimeout: "5m", RequestTimeout: "5m"},
		ServerOverrides: map[string]Override{}, SkillOverrides: map[string]Override{},
	}
}

func (l *loader) loadSettings() error {
	path := filepath.Join(l.agents, "agentmux.json")
	data, err := readBounded(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err = l.parseSettings(data); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (l *loader) parseSettings(data []byte) error {
	fields, err := object(data)
	if err != nil {
		return err
	}
	if err = allowFields(fields, "harnesses", "runtime", "serverOverrides", "skillOverrides"); err != nil {
		return err
	}
	settings := &l.config.Settings
	if raw, exists := fields["harnesses"]; exists {
		settings.Harnesses, err = parseHarnesses(raw)
		if err != nil {
			return err
		}
	}
	if raw, exists := fields["runtime"]; exists {
		if err = parseRuntime(raw, &settings.Runtime); err != nil {
			return err
		}
	}
	settings.ServerOverrides, err = parseOverrides(fields["serverOverrides"], false)
	if err != nil {
		return fmt.Errorf("serverOverrides: %w", err)
	}
	settings.SkillOverrides, err = parseOverrides(fields["skillOverrides"], true)
	if err != nil {
		return fmt.Errorf("skillOverrides: %w", err)
	}
	return nil
}

func parseRuntime(raw json.RawMessage, runtime *Runtime) error {
	fields, err := object(raw)
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}
	if err = allowFields(fields, "idleTimeout", "requestTimeout"); err != nil {
		return err
	}
	for _, name := range []string{"idleTimeout", "requestTimeout"} {
		if _, exists := fields[name]; !exists {
			continue
		}
		value, err := stringField(fields, name, true)
		if err != nil {
			return err
		}
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return fmt.Errorf("runtime.%s must be a positive Go duration, such as 5m", name)
		}
		if name == "idleTimeout" {
			runtime.IdleTimeout = value
		} else {
			runtime.RequestTimeout = value
		}
	}
	return nil
}

func parseHarnesses(raw json.RawMessage) ([]string, error) {
	values, err := stringList(raw, "harnesses")
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{"claude-code": true, "codex": true, "cursor": true, "opencode": true, "gemini": true, "vscode": true}
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !allowed[value] {
			return nil, fmt.Errorf("unsupported harness %q (choose claude-code, codex, cursor, opencode, gemini or vscode)", value)
		}
		if !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	return result, nil
}

func parseSharing(value string) (string, error) {
	if value != "shared" && value != "isolated" {
		return "", fmt.Errorf("sharing must be shared or isolated")
	}
	return value, nil
}

func parseOverrides(raw json.RawMessage, skills bool) (map[string]Override, error) {
	result := map[string]Override{}
	if raw == nil {
		return result, nil
	}
	fields, err := object(raw)
	if err != nil {
		return nil, err
	}
	for _, name := range sortedKeys(fields) {
		if !validEntryName(name) {
			return nil, fmt.Errorf("invalid override name %q", name)
		}
		entry, err := object(fields[name])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		allowed := []string{"harnesses"}
		if !skills {
			allowed = append(allowed, "sharing")
		}
		if err = allowFields(entry, allowed...); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		override := Override{}
		if value, exists := entry["harnesses"]; exists {
			override.Harnesses, err = parseHarnesses(value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
		if _, exists := entry["sharing"]; exists {
			override.Sharing, err = stringField(entry, "sharing", true)
			if err == nil {
				override.Sharing, err = parseSharing(override.Sharing)
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
		result[name] = override
	}
	return result, nil
}

func (l *loader) applyOverrides() {
	for _, name := range sortedKeys(l.config.Settings.ServerOverrides) {
		override := l.config.Settings.ServerOverrides[name]
		server, exists := l.config.Servers[name]
		if !exists {
			l.warn("serverOverrides", fmt.Errorf("%q does not match a loaded server", name))
			continue
		}
		if override.Harnesses != nil {
			server.Harnesses = override.Harnesses
		}
		if override.Sharing != "" {
			server.Sharing = override.Sharing
		}
		l.config.Servers[name] = server
	}
	found := map[string]bool{}
	for i := range l.config.Skills {
		skill := &l.config.Skills[i]
		override, exists := l.config.Settings.SkillOverrides[skill.Name]
		if !exists {
			continue
		}
		found[skill.Name] = true
		if override.Harnesses != nil {
			skill.Harnesses = override.Harnesses
		}
	}
	for _, name := range sortedKeys(l.config.Settings.SkillOverrides) {
		if !found[name] {
			l.warn("skillOverrides", fmt.Errorf("%q does not match a loaded skill", name))
		}
	}
}
