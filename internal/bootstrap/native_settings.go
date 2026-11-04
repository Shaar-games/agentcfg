package bootstrap

import "agentcfg/internal/config"

type nativeServerImport struct {
	server     config.Server
	warnings   []string
	advisories []string
}

// These client-side deadlines are not part of a portable upstream definition.
// Once imported, the broker applies agentcfg's own runtime deadlines instead.
// Keep the list explicit: auth, environment and tool policies affect behavior
// and must not be silently discarded as though they were cosmetic settings.
func ignorableNativeSetting(harness, key string) bool {
	switch harness {
	case "opencode", "gemini":
		return key == "timeout"
	case "codex":
		return key == "startup_timeout_sec" || key == "startup_timeout_ms" || key == "tool_timeout_sec"
	default:
		return false
	}
}
