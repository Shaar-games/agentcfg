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
- **Interactive Charmbracelet Huh forms** for harness selection, MCP sharing settings, skill availability, plugin inventory, diagnostics and sync confirmation.
- **Starts on demand and stops after inactivity**; no permanently installed background service.
- **Individual MCP entries** in each harness, not one aggregated tool catalog.
- **Global configuration** at `~/.agentmux/`, independent of the current project.
- **Skills and Agent Plugins** discovered from canonical directories and installed into native harness locations.
- **Selective distribution** using MCP harness filters and a skill's `metadata.harness`.
- **Conservative synchronization** with dry runs, backups, ownership tracking, and conflict detection.
- **Isolated mode** for MCPs that need per-client state or callbacks.

## Installation

The installers download **precompiled GitHub Release binaries**, verify the release archive against its published `SHA256SUMS`, and install it without building source code. **No Go, Git, Node.js, Python or mise installation is required.** Bash or PowerShell and standard download/archive utilities are sufficient.

**Linux / macOS (Bash)**

```sh
curl -fsSL https://raw.githubusercontent.com/Shaar-games/agentmux/main/install.sh | bash
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/Shaar-games/agentmux/main/install.ps1 | iex
```

The installer places a single executable in `~/.local/bin/agentmux` (`agentmux.exe` on Windows). No source checkout, compiler or persistent build directory is created. Add `~/.local/bin` to your `PATH` if necessary. Available release targets are Linux, macOS and Windows on AMD64 and ARM64 (where the OS supports the architecture). The scripts require `curl` and `tar` plus `sha256sum` or `shasum` on Unix; Windows PowerShell provides its own downloader, checksum and ZIP extraction.

The scripts **do not modify your harness configurations**. They refuse to overwrite an existing executable, validate SHA-256 checksums, and run a post-install binary smoke test. Review the remote scripts before executing them; these one-liners follow the repository's `main` branch. Set `AGENTMUX_VERSION=v0.2.1` to install a specific published release, or `AGENTMUX_BIN_DIR` for another executable directory. A checksum fetched from the same release prevents accidental corruption but is not a substitute for a cryptographically signed provenance attestation.

## Quick start

Run these commands from **any directory**. Launch `agentmux` with no arguments to open the interactive Huh configuration menus:

```sh
agentmux init                  # Create ~/.agentmux/ without overwriting existing files
# Edit ~/.agentmux/mcp.json and ~/.agentmux/agentmux.json
agentmux                       # Huh configuration menus
agentmux ui                    # Explicit interactive configuration
agentmux validate              # Validate sources, plugins, and skills
agentmux list                  # Show discovered MCP servers and skills
agentmux sync                  # Preview user-level changes (read-only)
agentmux sync --apply          # Apply reviewed changes
agentmux status                # Inspect the broker (does not start it)
```

When a harness next connects to a generated MCP entry, the proxy starts the broker if it is not already running. `agentmux stop` shuts down the broker and its managed MCP children.

### Interactive configuration

The Huh interface presents a main menu with **Harnesses**, **MCP servers**, **Skills**, **Agent Plugins**, **Diagnostics**, **Synchronization**, **Refresh**, and **Exit**. Use up/down arrows or `j`/`k` to navigate, `/` to filter choices and `Enter` to open or confirm. Huh provides a text-based accessible fallback for terminals without full-screen support. Listing status never starts MCP servers.

Select a harness to enable/disable it, an existing MCP server to change **shared** / **isolated**, or a skill to toggle individual harness availability through its source `metadata.harness`. The Huh sync menu first displays a read-only plan, then requires explicit confirmation before applying native user-level changes. **MCP server definitions (command, URL, credentials, arguments) and plugin package installation are not Huh editors:** configure those sources in `~/.agentmux/mcp.json` or `~/.agentmux/plugins/`, then validate before syncing. Huh only inventories plugin contents. The CLI commands `agentmux sync` (preview) and `agentmux sync --apply` (apply) are also available. Interactive terminal commands use Huh; scripts and pipelines retain plain output, and `--plain` explicitly suppresses Huh forms. `agentmux status --json` remains machine-readable; `agentmux sync --apply --yes` skips its confirmation, while `--plain` also preserves the non-interactive execution path.

> **Migrating existing setups?** Do not immediately replace your MCP configurations. The current release does not automatically import or adopt existing harness entries. Follow **[MIGRATION_FOR_AGENTS.md](MIGRATION_FOR_AGENTS.md)** for a backup-first, conflict-aware migration that an AI coding agent can execute with you.

## Global configuration

Everything originates from your **home directory**, not from whichever repository is open:

```text
~/.agentmux/
├── agentmux.json             # Enabled harnesses, runtime, MCP overrides
├── mcp.json                  # Canonical MCP definitions
├── AGENTS.md                 # Optional global instructions with harness filters
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

### OAuth for remote MCP servers

Agentmux uses the **official `github.com/modelcontextprotocol/go-sdk`** OAuth implementation for upstream Streamable HTTP MCP connections. OAuth is opt-in and never launches an authorization browser when an MCP starts automatically. Add an OAuth block to an HTTPS server entry (or to `serverOverrides` for a portable plugin), using a registered client ID or an explicitly supported registration method:

```json
{
  "mcpServers": {
    "private-docs": {
      "type": "streamable-http",
      "url": "https://mcp.example.com/mcp",
      "sharing": "shared",
      "oauth": {
        "clientId": "YOUR_PUBLIC_CLIENT_ID",
        "redirectUrl": "http://127.0.0.1:8731/callback",
        "account": "work",
        "scopes": ["docs.read"]
      }
    }
  }
}
```

```sh
agentmux auth login private-docs        # Explicit browser consent and PKCE; local callback is temporary
agentmux auth status private-docs       # Local credentials status, without exposing tokens
agentmux auth logout private-docs       # Forget cached credentials (not remote revocation)
```

Use `agentmux auth login private-docs --no-browser` to display the authorization URL without launching a browser; a complete callback URL can be entered manually when a local callback listener cannot be opened. Accounts can be separated with distinct `oauth.account` names and server definitions. **OAuth token values and client secrets must never be stored in `mcp.json`**. On Windows, credentials are encrypted for the user with DPAPI. On Linux/macOS, specify a 32-byte secret encryption key via `AGENTMUX_OAUTH_KEY` (base64 encoded), supplied by your password manager or secrets facility; agentmux will not write the encryption key to disk or fall back to plaintext. Do not change this key without first migrating existing credentials.

OAuth support is for HTTP upstream MCPs, not OAuth configuration embedded in portable Agent Plugins. Authorization success depends on each provider supporting the configured client registration, redirect URI, scopes and official MCP OAuth discovery endpoints. See the [configuration reference](docs/configuration.md) for validation rules and [transport details](docs/transports.md) for protocol limits.

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

An omitted `metadata.harness` distributes the skill to all selected, supported harnesses. With the field present, only the comma-separated harness IDs are selected. `metadata.harness: none` explicitly disables all harness destinations without removing the source. This is a **distribution rule**, not a security boundary: some clients also discover shared skill directories independently.

Plugin skills use the same frontmatter, located under `~/.agentmux/plugins/<plugin>/skills/`. Keep plugin manifests and MCP components in the [Agent Plugins format](https://agent-plugins.org/plugin-authors/mcp-servers).

### Global instructions — `~/.agentmux/AGENTS.md`

You can keep one globally shared set of agent instructions, optionally restricted to selected harnesses:

```markdown
---
metadata:
  harness: codex,claude-code,opencode
---

# Global coding instructions

Prefer small, testable changes and preserve existing project conventions.
```

During `agentmux sync --apply`, the instruction body is installed as `~/.codex/AGENTS.md` (Codex), `~/.claude/CLAUDE.md` (Claude Code), or `~/.config/opencode/AGENTS.md` (OpenCode). The routing frontmatter is stripped from the *generated copy*; the canonical source is never rewritten. Without `metadata.harness`, all globally enabled harnesses **with verified instructions adapters** are selected. Use `metadata.harness: none` to disable distribution. Existing unmanaged destination files are never overwritten, and changes to previously managed files require an unmodified ownership record. Other clients currently receive a warning rather than a guessed global instruction location. All paths are in the user home, never in an open project.

## Supported harnesses

| Harness | User MCP configuration | User skills destination |
| --- | --- | --- |
| Claude Code (`claude-code`) | `~/.claude.json` | `~/.claude/skills/` |
| Codex (`codex`) | `~/.codex/config.toml` | `~/.agents/skills/` |
| Cursor (`cursor`) | `~/.cursor/mcp.json` | `~/.cursor/skills/` |
| OpenCode (`opencode`) | `~/.config/opencode/opencode.json` (or existing JSONC) | `~/.config/opencode/skills/` |
| Gemini CLI (`gemini`) | `~/.gemini/settings.json` | `~/.gemini/skills/` |
| Google Antigravity App (`antigravity`) | `~/.gemini/config/mcp_config.json` | `~/.gemini/config/skills/` |

The generated MCP entries run `agentmux connect --server <name> --harness <client>` using the **installed executable's absolute path**. Clients retain distinct MCP names and their native config formats.

`claude-desktop` does **not** have an adapter yet; it is not interchangeable with Claude Code. Antigravity's CLI also discovers `~/.gemini/antigravity-cli/skills/`, but the App and CLI share the `~/.gemini/config/` personal layout; the current `antigravity` adapter targets that common layout. VS Code's existing adapter is project-scoped only and therefore unavailable in the current global-only CLI.

## Migrate existing MCPs and skills

Use the root-level **[AI agent migration guide](MIGRATION_FOR_AGENTS.md)**. It also explains why old project-local `.agents/` or `.agentmux/` source directories are not automatically imported. It gives an AI coding agent an explicit, safe workflow to:

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
- Sharing is not isolation: the server's application state can be shared between harnesses. Use sharing: "isolated" for private state, roots, sampling, elicitation, or client-specific callbacks.
- **Protocol scope:** MCP `2026-07-28` request-scoped metadata, `server/discover`, stateless HTTP headers, and JSON/SSE responses use the official Go MCP SDK bridge alongside legacy 2025 handshakes. Modern protocol messages are routed per request; the broker does not merge modern clients into an initialized legacy stdio session. OAuth authorization is **explicit**, not an automatic login during MCP startup. Legacy standalone SSE transport and SSE cursor resumption are not implemented. Modern subscriptions and multi-round-trip tool results depend on the capabilities of both the upstream and the calling harness.
- **Not implemented:** bidirectional sync and automatic migration/import of existing MCP definitions. Global instructions are supported only for verified user-scope Codex, Claude Code and OpenCode destinations; other instruction targets are not guessed.

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

GitHub Actions tests Linux, Windows and macOS. Tagged releases build precompiled AMD64/ARM64 binaries for all three operating systems, publish SHA-256 checksums, and are consumed by the installation scripts. See [release process](docs/releases.md).

Explore the [configuration reference](docs/configuration.md), [synchronization behavior](docs/synchronization.md), [architecture](docs/architecture.md), [transports](docs/transports.md), [verification notes](docs/verification.md), and [roadmap](docs/roadmap.md).

<sub>Header and GitHub badges rendered with <a href="https://github.com/jal-co/shieldcn">shieldcn</a>. The README uses hosted SVGs; it does not require shieldcn at runtime.</sub>
