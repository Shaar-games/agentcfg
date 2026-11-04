package bootstrap

import (
	"encoding/json"
	"path"
	"slices"
	"strings"
)

// This desktop-owned runtime uses per-session CUA pipes and access context.
// Recognize its executable AND private configuration, never its name alone.
// Keeping it native preserves the app integration without copying pipe values
// into canonical user MCP sources or sharing one session with another harness.
func codexDesktopRuntime(harness, name string, fields map[string]json.RawMessage) bool {
	if harness != "codex" || name != "node_repl" {
		return false
	}
	command, err := stringValue(fields["command"])
	if err != nil {
		return false
	}
	command = strings.ToLower(strings.ReplaceAll(command, `\`, "/"))
	if !strings.Contains(command, "/openai/codex/runtimes/cua_node/") || (path.Base(command) != "node_repl.exe" && path.Base(command) != "node_repl") {
		return false
	}
	args, err := stringList(fields["args"])
	if err != nil || len(args) != 0 {
		return false
	}
	vars, err := stringList(fields["env_vars"])
	if err != nil || !slices.Contains(vars, "BROWSER_USE_ACCESS_PIPE_PATH") {
		return false
	}
	env, err := stringMap(fields["env"])
	return err == nil && env["CODEX_CLI_PATH"] != "" && env["NODE_REPL_NODE_PATH"] != "" &&
		env["SKY_CUA_NATIVE_PIPE"] == "1" && strings.HasPrefix(env["SKY_CUA_NATIVE_PIPE_DIRECTORY"], `\\.\pipe\codex-computer-use-`)
}
