// Package syncer projects canonical MCP and skills into native harness files.
// It never executes MCP commands and never writes anything while planning.
package syncer

import (
	"fmt"
	"os"
	"path/filepath"
)

type target struct {
	name, path, key, skills string
	toml                    bool
}

func registry(base, scope string) []target {
	claude := ".mcp.json"
	opencode := "opencode.json"
	if scope == "user" {
		claude = ".claude.json"
		opencode = OpenCodeUserConfigPaths()[0]
		for _, candidate := range OpenCodeUserConfigPaths() {
			if _, err := os.Stat(filepath.Join(base, candidate)); err == nil {
				opencode = candidate
				break
			}
		}
	}
	if _, err := os.Stat(filepath.Join(base, opencode+"c")); err == nil {
		if _, err := os.Stat(filepath.Join(base, opencode)); os.IsNotExist(err) {
			opencode += "c"
		}
	}
	values := []target{
		{name: "claude-code", path: claude, key: "mcpServers", skills: ".claude/skills"},
		{name: "cursor", path: ".cursor/mcp.json", key: "mcpServers", skills: ".cursor/skills"},
		{name: "codex", path: ".codex/config.toml", key: "mcp_servers", skills: ".agents/skills", toml: true},
		{name: "opencode", path: opencode, key: "mcp", skills: ".opencode/skills"},
		{name: "gemini", path: ".gemini/settings.json", key: "mcpServers", skills: ".gemini/skills"},
		{name: "antigravity", path: ".gemini/config/mcp_config.json", key: "mcpServers", skills: ".gemini/config/skills"},
		{name: "vscode", path: ".vscode/mcp.json", key: "servers", skills: ".github/skills"},
	}
	if scope == "user" {
		for i := range values {
			if values[i].name == "codex" {
				values[i].skills = ".codex/skills"
			}
			if values[i].name == "opencode" {
				values[i].skills = ".config/opencode/skills"
			}
		}
	}
	for i := range values {
		values[i].path = filepath.Join(base, filepath.FromSlash(values[i].path))
		values[i].skills = filepath.Join(base, filepath.FromSlash(values[i].skills))
	}
	return values
}

// Preferred and legacy user locations are shared by discovery, projection and
// restore. A lone existing legacy config stays active instead of being bypassed.
func OpenCodeUserConfigPaths() []string {
	return []string{filepath.Join(".config", "opencode", "opencode.json"), filepath.Join(".config", "opencode", "opencode.jsonc"),
		filepath.Join(".opencode", "opencode.json"), filepath.Join(".opencode", "opencode.jsonc")}
}

// A harness has one skills destination but may read multiple user MCP files.
// Keep this expansion shared by init adoption, link preparation and normal sync.
func configTargets(base, scope string, primary target) []target {
	if scope != "user" || primary.name != "opencode" {
		return []target{primary}
	}
	var result []target
	for _, relative := range OpenCodeUserConfigPaths() {
		candidate := primary
		candidate.path = filepath.Join(base, relative)
		result = append(result, candidate)
	}
	return result
}

func proxyEntry(t target, binary, root, name string) map[string]any {
	args := []string{"connect", "--server", name, "--harness", t.name}
	if t.name == "opencode" {
		return map[string]any{"type": "local", "command": append([]string{binary}, args...), "enabled": true}
	}
	entry := map[string]any{"command": binary, "args": args}
	if t.name != "codex" && t.name != "gemini" && t.name != "antigravity" {
		entry["type"] = "stdio"
	}
	return entry
}

func selectTargets(all []target, names []string, scope string) (map[string]bool, error) {
	known := map[string]bool{}
	for _, t := range all {
		known[t.name] = true
	}
	selected := map[string]bool{}
	for _, name := range names {
		if !known[name] {
			return nil, fmt.Errorf("unsupported harness %q", name)
		}
		if name == "vscode" && scope == "user" {
			return nil, fmt.Errorf("vscode user scope requires a platform-specific profile; use project scope in this release")
		}
		selected[name] = true
	}
	return selected, nil
}
