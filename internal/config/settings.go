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
		ServerOverrides: map[string]Override{},
	}
}

func (l *loader) loadSettings() error {
	path := filepath.Join(l.agents, "agentcfg.json")
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
	if err = allowFields(fields, "harnesses", "runtime", "serverOverrides"); err != nil {
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
	return validateHarnessNames(values)
}

// parseServerHarnesses accepts the explicit empty selection only for MCP
// filters. User-level harness selection continues to use parseHarnesses.
func parseServerHarnesses(raw json.RawMessage) ([]string, error) {
	values, err := stringList(raw, "harnesses")
	if err != nil || values == nil {
		return values, err
	}
	for _, value := range values {
		if value == "none" {
			if len(values) != 1 {
				return nil, fmt.Errorf("harnesses: none must be the only entry")
			}
			return values, nil
		}
	}
	return validateHarnessNames(values)
}

func validateHarnessNames(values []string) ([]string, error) {
	allowed := map[string]bool{"claude-code": true, "codex": true, "cursor": true, "opencode": true, "gemini": true, "antigravity": true, "vscode": true}
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !allowed[value] {
			return nil, fmt.Errorf("unsupported harness %q (choose claude-code, codex, cursor, opencode, gemini, antigravity or vscode)", value)
		}
		if !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	return result, nil
}

// ValidateUserHarnesses is used when initializing user-global configuration.
// Unlike the read-only loader (which recognizes legacy VS Code settings), the
// initializer must not create a harness selection it cannot synchronize.
// It applies the same validation and stable duplicate removal as config.Load.
func ValidateUserHarnesses(values []string) ([]string, error) {
	if values == nil {
		values = []string{}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	selected, err := parseHarnesses(encoded)
	if err != nil {
		return nil, err
	}
	for _, name := range selected {
		if name == "vscode" {
			return nil, fmt.Errorf("vscode cannot be synchronized in user scope; no files created")
		}
	}
	return selected, nil
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
			allowed = append(allowed, "sharing", "oauth")
		}
		if err = allowFields(entry, allowed...); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		override := Override{}
		if raw, exists := entry["oauth"]; exists {
			override.OAuth, err = parseOAuth(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
		if value, exists := entry["harnesses"]; exists {
			if skills {
				override.Harnesses, err = parseHarnesses(value)
			} else {
				override.Harnesses, err = parseServerHarnesses(value)
			}
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
		if override.OAuth != nil {
			server.OAuth = override.OAuth
		}
		if err := validateServerOAuth(server); err != nil {
			l.warn("serverOverrides."+name, err)
			delete(l.config.Servers, name)
			continue
		}
		l.config.Servers[name] = server
	}
}
