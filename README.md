<p align="center">
  <img src="https://shieldcn.dev/header/glow.svg?title=agentmux&amp;subtitle=One+MCP+process.+Every+AI+agent.&amp;logo=go&amp;theme=blue&amp;size=banner" alt="agentmux — shared MCP processes and skills for AI coding agents" width="750" />
</p>

<p align="center">
  <a href="https://github.com/Shaar-games/agentmux/actions"><img alt="GitHub CI" src="https://shieldcn.dev/github/ci/Shaar-games/agentmux.svg?variant=outline&amp;size=sm" /></a>
  <a href="https://github.com/Shaar-games/agentmux/stargazers"><img alt="GitHub stars" src="https://shieldcn.dev/github/stars/Shaar-games/agentmux.svg?variant=outline&amp;size=sm" /></a>
  <a href="https://github.com/Shaar-games/agentmux/issues"><img alt="GitHub issues" src="https://shieldcn.dev/github/issues/Shaar-games/agentmux.svg?variant=outline&amp;size=sm" /></a>
  <img alt="Go 1.26" src="https://shieldcn.dev/badge/Go-1.26-blue.svg?variant=outline&amp;size=sm&amp;logo=go" />
</p>

<p align="center">
  <strong>Run each MCP server once. Connect it to multiple AI coding agents.</strong><br />
  A local, on-demand MCP process multiplexer and skills synchronizer written in Go.
</p>

<p align="center">
  <a href="#installation">Install</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="MIGRATION_FOR_AGENTS.md">Migrate with an AI agent</a> ·
  <a href="docs/architecture.md">Architecture</a>
</p>

---

## Why agentmux?

Most MCP clients launch their own copy of every configured local server. When Claude Code, Codex, Cursor, OpenCode, or Gemini CLI use the same tools, that can mean duplicate processes, startup overhead, and memory usage.

**agentmux keeps a separate, native MCP entry in every client but shares the upstream process.** Each client starts a lightweight stdio proxy; the proxy connects to a local broker, which launches the actual MCP server only when needed.

```text
Claude Code ── chrome-devtools: agentmux connect ──┐
Codex       ── chrome-devtools: agentmux connect ──┼── local IPC ── agentmux broker ── chrome-devtools × 1
Cursor      ── chrome-devtools: agentmux connect ──┘

Codex       ── context7: agentmux connect ────────┐
OpenCode    ── context7: agentmux connect ────────┴── local IPC ── agentmux broker ── Context7
```

**No local HTTP server or listening TCP port.** The broker uses a private Windows named pipe or Unix domain socket. Remote MCP servers can still be accessed through outbound Streamable HTTP connections.

### Features

- **One upstream per shared MCP configuration**, even with multiple connected harnesses.
- **Starts on demand and stops after inactivity**; no permanently installed background service.
- **Individual MCP entries** in each harness, not one aggregated tool catalog.
- **Global configuration** at `~/.agentmux/`, independent of the current project.
- **Skills and Agent Plugins** discovered from canonical directories and installed into native harness locations.
- **Selective distribution** using MCP harness filters and a skill's `metadata.harness`.
- **Conservative synchronization** with dry runs, backups, ownership tracking, and conflict detection.
- **Isolated mode** for MCPs that need per-client state or callbacks.

## Installation

Requires [Git](https://git-scm.com/) and [mise](https://mise.jdx.dev/getting-started.html). The installers use `mise.toml` to install the required Go toolchain and compile agentmux locally.

**Linux / macOS (POSIX shell)**

```sh
curl -fsSL https://raw.githubusercontent.com/Shaar-games/agentmux/main/install.sh | sh
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/Shaar-games/agentmux/main/install.ps1 | iex
```

Both installers keep the source and build files in `~/.local/share/agentmux/` and place an executable at `~/.local/bin/agentmux` (`agentmux.exe` on Windows). Unix uses a symlink; Windows uses a copy. Add `~/.local/bin` to your `PATH` if necessary.

The scripts **do not modify your harness configurations**. They refuse to overwrite an existing installation or executable. Review the remote installation scripts before executing them; these one-liners follow the repository's `main` branch.

## Quick start

Run these commands from **any directory**:

```sh
agentmux init                  # Create ~/.agentmux/ without overwriting existing files
# Edit ~/.agentmux/mcp.json and ~/.agentmux/agentmux.json
agentmux validate              # Validate sources, plugins, and skills
agentmux list                  # Show discovered MCP servers and skills
agentmux sync                  # Preview user-level changes (read-only)
agentmux sync --apply          # Apply reviewed changes
agentmux status                # Inspect the broker (does not start it)
```

When a harness next connects to a generated MCP entry, the proxy starts the broker if it is not already running. `agentmux stop` shuts down the broker and its managed MCP children.

> **Migrating existing setups?** Do not immediately replace your MCP configurations. The current release does not automatically import or adopt existing harness entries. Follow **[MIGRATION_FOR_AGENTS.md](MIGRATION_FOR_AGENTS.md)** for a backup-first, conflict-aware migration that an AI coding agent can execute with you.

## Global configuration

Everything originates from your **home directory**, not from whichever repository is open:

```text
~/.agentmux/
├── agentmux.json             # Enabled harnesses, runtime, MCP overrides
├── mcp.json                  # Canonical MCP definitions
├── AGENTS.md                 # Reserved for future instruction synchronization
├── skills/
│   └── code-review/
│       └── SKILL.md
└── plugins/
    └── docs-helper/
        ├── plugin.json      # Standard Agent Plugins manifest
        ├── mcp.json         # Standard plugin MCP component
        └── skills/
            └── documentation-check/
                └── SKILL.md
```

The source directories are not placed inside projects. `agentmux sync --apply` writes **user-level harness configuration** only. The application may also create runtime data and synchronization state beneath `~/.agentmux/`.

### MCP servers — `~/.agentmux/mcp.json`

```json
{
  "mcpServers": {
    "chrome-devtools": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "chrome-devtools-mcp@latest"],
      "sharing": "shared",
      "harnesses": ["claude-code", "codex", "cursor"]
    },
    "context7": {
      "type": "streamable-http",
      "url": "https://mcp.context7.com/mcp",
      "sharing": "shared"
    }
  }
}
```

For reproducible setups, pin server versions instead of using `@latest`. On Windows, npm-based commands may run through `.cmd` wrappers; see [transport limitations](docs/transports.md). A shared browser MCP shares its browser state too: choose `"sharing": "isolated"` if different agents must control independent sessions.

### Harness settings — `~/.agentmux/agentmux.json`

```json
{
  "harnesses": ["claude-code", "codex", "cursor", "opencode", "gemini"],
  "runtime": {
    "idleTimeout": "5m",
    "requestTimeout": "5m"
  },
  "serverOverrides": {
    "context7": {
      "harnesses": ["codex", "claude-code"],
      "sharing": "isolated"
    }
  }
}
```

`serverOverrides` is especially useful for **portable Agent Plugins**, whose `mcp.json` should remain standards-compliant. For example, a plugin server called `docs-helper.context7` can be filtered by adding an override under that qualified name. An override takes effect only if the named server is installed; otherwise validation reports a warning. There is **no `skillOverrides` setting**.

### Skills — `~/.agentmux/skills/<name>/SKILL.md`

Use standard [Agent Skills](https://agentskills.io/specification) frontmatter, with an optional agentmux-specific string in `metadata`:

```yaml
---
name: code-review
description: Review source code and explain actionable improvements.
metadata:
  harness: codex,claude-code,cursor
---

Review the requested code and report concrete findings.
```

An omitted `metadata.harness` distributes the skill to all selected, supported harnesses. With the field present, only the comma-separated harness IDs are selected. This is a **distribution rule**, not a security boundary: some clients also discover shared skill directories independently.

Plugin skills use the same frontmatter, located under `~/.agentmux/plugins/<plugin>/skills/`. Keep plugin manifests and MCP components in the [Agent Plugins format](https://agent-plugins.org/plugin-authors/mcp-servers).

## Supported harnesses

| Harness | User MCP configuration | User skills destination |
| --- | --- | --- |
| Claude Code (`claude-code`) | `~/.claude.json` | `~/.claude/skills/` |
| Codex (`codex`) | `~/.codex/config.toml` | `~/.agents/skills/` |
| Cursor (`cursor`) | `~/.cursor/mcp.json` | `~/.cursor/skills/` |
| OpenCode (`opencode`) | `~/.config/opencode/opencode.json` (or existing JSONC) | `~/.config/opencode/skills/` |
| Gemini CLI (`gemini`) | `~/.gemini/settings.json` | `~/.gemini/skills/` |

The generated MCP entries run `agentmux connect --server <name> --harness <client>` using the **installed executable's absolute path**. Clients retain distinct MCP names and their native config formats.

`claude-desktop` does **not** have an adapter yet; it is not interchangeable with Claude Code. VS Code's existing adapter is project-scoped only and therefore unavailable in the current global-only CLI.

## Migrate existing MCPs and skills

Use the root-level **[AI agent migration guide](MIGRATION_FOR_AGENTS.md)**. It gives an AI coding agent an explicit, safe workflow to:

1. Inspect **user-level** MCP and skill configurations without touching project files.
2. Back up the originals and translate selected definitions into `~/.agentmux/`.
3. Resolve name collisions before applying any generated entries.
4. Run `agentmux validate`, review the `agentmux sync` preview, then apply with approval.
5. Verify clients, MCP startup, process reuse, and the rollback path.

**Suggested prompt for your coding agent:**

> Read https://github.com/Shaar-games/agentmux/blob/main/MIGRATION_FOR_AGENTS.md and migrate my existing **user-level** MCP servers and skills to agentmux. Do not change any project files. Preserve my current configurations, show conflicts and a proposed diff, and obtain my approval before replacing old MCP entries or running `agentmux sync --apply`.

## Safety and current limits

- **No silent takeover:** unknown MCP names and unmanaged skill directories are preserved; collisions stop synchronization. Existing configurations are **not** imported automatically.
- **No project changes:** the public CLI uses `~/.agentmux/` and user-level targets. There is no `--root` flag or project-scope synchronization.
- **Not a sandbox:** installed MCP servers and plugin executables run with your account's permissions. Review untrusted packages and avoid committing credentials.
- **Sharing is not isolation:** the server's application state can be shared between harnesses. Use `sharing: "isolated"` for private state, roots, sampling, elicitation, or client-specific callbacks.
- **Protocol scope:** outbound Streamable HTTP supports ordinary 2025 JSON/SSE flows, without automatic OAuth, legacy standalone SSE transport, or SSE stream resumption.
- **Not implemented:** bidirectional sync, automatic migration/import, and propagation of `~/.agentmux/AGENTS.md` into harness instruction files. That file is a planned source, not a currently synchronized feature.

## Development and documentation

```sh
git clone https://github.com/Shaar-games/agentmux.git
cd agentmux
mise trust
mise install
mise run check
mise run build
```

Go tests include a real subprocess integration fixture that verifies two harness proxies can reuse one upstream MCP process. Test results do **not** imply that every version of every harness has been end-to-end tested.

Explore the [configuration reference](docs/configuration.md), [synchronization behavior](docs/synchronization.md), [architecture](docs/architecture.md), [transports](docs/transports.md), [verification notes](docs/verification.md), and [roadmap](docs/roadmap.md).

<sub>Header and GitHub badges rendered with <a href="https://github.com/jal-co/shieldcn">shieldcn</a>. The README uses hosted SVGs; it does not require shieldcn at runtime.</sub>
