# agentcfg

Go CLI for local MCP process sharing and declarative harness/skill configuration.

- Canonical sources are strictly user-level: `~/.agentcfg/mcp.json`,
  `~/.agentcfg/agentcfg.json`, `~/.agentcfg/skills/`, and `~/.agentcfg/plugins/`.
- Never use a project's `.agentcfg/` or `.agents/` as canonical sources or
  synchronize into project-local harness configuration.
- Agent Plugins manifests and MCP files must retain the portable standard format.
- Never modify live user harness configs in tests; use temporary directories.
- Keep stdout clean for MCP stdio; diagnostics go to stderr.
- Preserve unrelated config keys and user-modified managed entries.
- No listening TCP/HTTP server. Use Unix sockets and Windows named pipes.
- Run `go test ./...` and `go vet ./...` before considering a change verified.
- Bidirectional import/sync is deferred; do not silently overwrite manual edits.
