<p align="center">
  <img src="https://shieldcn.dev/header/glow.svg?title=agentcfg&amp;subtitle=One+MCP+process.+Every+AI+agent.&amp;logo=go&amp;theme=blue&amp;size=banner" alt="agentcfg — shared MCP processes and skills for AI coding agents" width="750" />
</p>

<p align="center">
  <a href="https://github.com/Shaar-games/agentcfg/actions"><img alt="GitHub CI" src="https://shieldcn.dev/github/ci/Shaar-games/agentcfg.svg?variant=outline&amp;size=sm" /></a>
  <a href="https://github.com/Shaar-games/agentcfg/stargazers"><img alt="GitHub stars" src="https://shieldcn.dev/github/stars/Shaar-games/agentcfg.svg?variant=outline&amp;size=sm" /></a>
  <a href="https://github.com/Shaar-games/agentcfg/issues"><img alt="GitHub issues" src="https://shieldcn.dev/github/issues/Shaar-games/agentcfg.svg?variant=outline&amp;size=sm" /></a>
  <img alt="Go 1.26" src="https://shieldcn.dev/badge/Go-1.26-blue.svg?variant=outline&amp;size=sm&amp;logo=go" />
</p>

<p align="center">
  <strong>Run each MCP server once. Connect it to multiple AI coding agents.</strong><br />
  A local, on-demand MCP process multiplexer and skills synchronizer written in Go.
</p>

<p align="center">
  <a href="#installation">Install</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="docs/configuration.md">Configuration</a> ·
  <a href="docs/architecture.md">Architecture</a>
</p>

---

## Why agentcfg?

Most MCP clients launch their own copy of every configured local server. When Claude Code, Codex, Cursor, OpenCode, or Gemini CLI use the same tools, that can mean duplicate processes, startup overhead, and memory usage.

**agentcfg keeps a separate, native MCP entry in every client but shares the upstream process.** Each client starts a lightweight stdio proxy; the proxy connects to a local broker, which launches the actual MCP server only when needed.

```text
Claude Code ── chrome-devtools: agentcfg connect ──┐
Codex       ── chrome-devtools: agentcfg connect ──┼── local IPC ── agentcfg broker ── chrome-devtools × 1
Cursor      ── chrome-devtools: agentcfg connect ──┘

Codex       ── context7: agentcfg connect ────────┐
OpenCode    ── context7: agentcfg connect ────────┴── local IPC ── agentcfg broker ── Context7
```

**No local HTTP server or listening TCP port.** The broker uses a private Windows named pipe or Unix domain socket. Remote MCP servers can still be accessed through outbound Streamable HTTP connections.

### Features

- **One upstream per shared MCP configuration**, even with multiple connected harnesses.
- **Consistent Charmbracelet Huh lists** for harness selection, per-harness MCP and skill availability, MCP sharing settings, plugin inventory, diagnostics and sync confirmation.
- **Native setup discovery** to inspect installed harness MCPs and skills before creating a canonical configuration.
- **Shell completion** for Bash, Zsh, Fish and PowerShell (including `pwsh`).
- **Starts on demand and stops after inactivity**; no permanently installed background service.
- **Individual MCP entries** in each harness, not one aggregated tool catalog.
- **Canonical configuration** at `~/.agentcfg/`, projected into each harness's native configuration files.
- **Skills and Agent Plugins** discovered from canonical directories and installed into native harness locations.
- **Selective distribution** using MCP harness filters and a skill's `metadata.harness`.
- **Conservative synchronization** with dry runs, backups, ownership tracking, and conflict detection.
- **Isolated mode** for MCPs that need per-client state or callbacks.

## Installation

The installers download **precompiled GitHub Release binaries**, verify the release archive against its published `SHA256SUMS`, and install it without building source code. **No Go, Git, Node.js, Python or mise installation is required.** Bash or PowerShell and standard download/archive utilities are sufficient.

**Linux / macOS (Bash)**

```sh
curl -fsSL https://raw.githubusercontent.com/Shaar-games/agentcfg/main/install.sh | bash
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/Shaar-games/agentcfg/main/install.ps1 | iex
```

The installer places the executable in `~/.local/bin/agentcfg` (`agentcfg.exe` on Windows). No source checkout, compiler or persistent build directory is created. Add `~/.local/bin` to your `PATH` if necessary. Available release targets are Linux, macOS and Windows on AMD64 and ARM64 (where the OS supports the architecture). The scripts require `curl` and `tar` plus `sha256sum` or `shasum` on Unix; Windows PowerShell provides its own downloader, checksum and ZIP extraction.

The scripts **do not change MCP or harness settings during installation**. They verify SHA-256 checksums, validate the downloaded binary and **replace an existing agentcfg executable** when reinstalling. The previous binary is retained for rollback until validation succeeds; a Windows executable still in use may leave a temporary backup. Review the remote scripts before executing them; these one-liners follow the repository's `main` branch. Set `AGENTCFG_VERSION` to an existing published tag to pin a release, or `AGENTCFG_BIN_DIR` for another executable directory. A checksum fetched from the same release prevents accidental corruption but is not a substitute for a cryptographically signed provenance attestation.

To update an existing installation to the **latest published stable GitHub Release built from `main`**, run:

```sh
agentcfg update
```

Self-update downloads the platform's release archive and `SHA256SUMS`, verifies the checksum, tests the staged executable and replaces the running binary with rollback protection. On Windows, a previous executable still in use may be retained as a backup until it can be removed after the command exits. This updates only agentcfg itself, not MCP definitions or native harness settings.

## Quick start

Run these commands from **any directory**. `agentcfg` without arguments prints the command help; the interactive Huh configuration starts with `agentcfg config`:

```sh
agentcfg                       # Display help without changing configuration
agentcfg init                  # Discover native MCPs/skills; choose and confirm canonical setup
agentcfg init --plain          # Read-only discovery preview, without Huh or writes
agentcfg init --apply          # Also preview and confirm native harness synchronization
agentcfg config                # Open Huh configuration for harnesses, skills, MCPs and sync
agentcfg harness               # Configure enabled harnesses (alias: harnesses)
agentcfg skill                 # Configure skill availability (alias: skills)
agentcfg mcp                   # Configure MCP availability and sharing (alias: mcps)
agentcfg mcp --plain           # Inspect effective MCP availability without editing
agentcfg validate              # Validate sources, plugins, and skills
agentcfg list                  # Show discovered MCP servers and skills
agentcfg update                # Update this executable from GitHub Releases
agentcfg sync                  # Preview user-level changes (read-only)
agentcfg sync --apply          # Apply reviewed changes
agentcfg status                # Inspect the broker (does not start it)
agentcfg completion bash       # Print a sourceable completion script
```

When a harness next connects to a generated MCP entry, the proxy starts the broker if it is not already running. `agentcfg stop` shuts down the broker and its managed MCP children.

### First-run discovery and confirmation

`agentcfg init` examines the **user-level native MCP configurations and skill directories** of supported, installed harnesses. It prepares a proposed `~/.agentcfg/mcp.json`, `~/.agentcfg/agentcfg.json` and skill inventory, identifies conflicts or settings that cannot safely be adopted, and displays a preview **before writing**. Discovery must not start MCP servers, execute skill scripts, or alter native harness files.

Review the discovered entries, target harnesses, and any duplicates in the Huh interface. **`agentcfg init` asks for explicit confirmation before creating the canonical sources**; cancelling stops the operation. `agentcfg init --apply` additionally previews the proposed native changes and asks for a **second Huh confirmation**, including any adoption of existing scanner-proven MCP entries or identical skill trees. The original native files are verified again and backed up; unrelated settings are preserved. Unproved collisions and native TOML MCP adoption are refused: **there is no silent overwrite**. For read-only automation, `agentcfg init --plain` prints the discovery preview; `--yes` explicitly approves the creation and, with `--apply`, synchronization in noninteractive use. Independently, `agentcfg sync` previews ongoing configuration changes and `agentcfg sync --apply` applies a reviewed plan. See [CLI setup details](docs/cli.md) and [synchronization rules](docs/synchronization.md).

### Interactive configuration

Run `agentcfg config` to manage **harnesses, MCP servers, skills, Agent Plugins, diagnostics and synchronization** through consistent native Huh checkbox lists, including the top-level navigation and MCP sharing choices. Use ↑/↓ to navigate, **Space to check a section, then Enter to open it**. Navigation allows one checked section at a time; Enter with no section checked returns to the previous screen. Harness and skill availability allow multiple checked values, with the current settings prechecked; Enter saves, Ctrl+C cancels. MCP servers and skills can be selected from the same style of list to view details or change supported settings. `agentcfg harness` / `agentcfg harnesses`, `agentcfg skill` / `agentcfg skills` and `agentcfg mcp` / `agentcfg mcps` open those sections directly. Run `agentcfg` or `agentcfg help` to see the command reference. Huh provides an accessible numbered prompt fallback for terminals without full-screen support. On Windows, genuine interactive consoles use the normal Huh interface even when `TERM=dumb`; set `AGENTCFG_ACCESSIBLE=1` to request numbered prompts instead. Listing status never starts MCP servers.

Use **Huh multi-select checkboxes** to configure enabled harnesses and the availability of each skill or MCP. Choose **shared** / **isolated** separately for an existing MCP server. The Huh sync menu first displays a read-only plan and asks before applying changes to native harness configuration files. **Raw MCP definitions (command, URL, credentials, arguments) and plugin package installation still require editing `~/.agentcfg/mcp.json` or `~/.agentcfg/plugins/`**; validate these sources before syncing. Huh inventories plugin contents. For `harness(es)`, `skill(s)` and `mcp(s)`, `--plain` prints the effective configuration without opening a form or editing files. It also keeps inspection and sync previews script-friendly. `agentcfg status --json` remains machine-readable and `agentcfg sync --apply --yes` skips confirmation.

### Shell completion

`agentcfg completion bash|zsh|fish|powershell|pwsh` prints a completion script for that shell. For instance, run `source <(agentcfg completion bash)` in Bash, `agentcfg completion fish | source` in Fish, or `agentcfg completion pwsh | Out-String | Invoke-Expression` in PowerShell 7. PowerShell 5.1 uses `powershell` instead of `pwsh`. For persistent installation in your shell's startup file, see [shell completion setup](docs/completion.md).

## Global configuration

Canonical MCP definitions, harness preferences, plugins and skills live under `~/.agentcfg/`:

```text
~/.agentcfg/
├── agentcfg.json             # Enabled harnesses, runtime, MCP overrides
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

`agentcfg sync --apply` writes the selected harnesses' **native configuration files** (listed below). Runtime data and synchronization state live beneath `~/.agentcfg/`.

### MCP servers — `~/.agentcfg/mcp.json`

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

agentcfg uses the **official `github.com/modelcontextprotocol/go-sdk`** OAuth implementation for upstream Streamable HTTP MCP connections. OAuth is opt-in and never launches an authorization browser when an MCP starts automatically. Add an OAuth block to an HTTPS server entry (or to `serverOverrides` for a portable plugin), using a registered client ID or an explicitly supported registration method:

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
agentcfg auth login private-docs        # Explicit browser consent and PKCE; local callback is temporary
agentcfg auth status private-docs       # Local credentials status, without exposing tokens
agentcfg auth logout private-docs       # Forget cached credentials (not remote revocation)
```

Use `agentcfg auth login private-docs --no-browser` to display the authorization URL without launching a browser; a complete callback URL can be entered manually when a local callback listener cannot be opened. Accounts can be separated with distinct `oauth.account` names and server definitions. **OAuth token values and client secrets must never be stored in `mcp.json`**. On Windows, credentials are encrypted for the user with DPAPI. On Linux/macOS, specify a 32-byte secret encryption key via `AGENTCFG_OAUTH_KEY` (base64 encoded), supplied by your password manager or secrets facility; agentcfg will not write the encryption key to disk or fall back to plaintext. Keep this encryption key available to access stored credentials.

OAuth support is for HTTP upstream MCPs, not OAuth configuration embedded in portable Agent Plugins. Authorization success depends on each provider supporting the configured client registration, redirect URI, scopes and official MCP OAuth discovery endpoints. See the [configuration reference](docs/configuration.md) for validation rules and [transport details](docs/transports.md) for protocol limits.

### Harness settings — `~/.agentcfg/agentcfg.json`

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

### Skills — `~/.agentcfg/skills/<name>/SKILL.md`

Use standard [Agent Skills](https://agentskills.io/specification) frontmatter, with an optional agentcfg-specific string in `metadata`:

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

Plugin skills use the same frontmatter, located under `~/.agentcfg/plugins/<plugin>/skills/`. Keep plugin manifests and MCP components in the [Agent Plugins format](https://agent-plugins.org/plugin-authors/mcp-servers).

### Global instructions — `~/.agentcfg/AGENTS.md`

You can keep one globally shared set of agent instructions, optionally restricted to selected harnesses:

```markdown
---
metadata:
  harness: codex,claude-code,opencode
---

# Global coding instructions

Prefer small, testable changes and preserve existing project conventions.
```

During `agentcfg sync --apply`, the instruction body is installed as `~/.codex/AGENTS.md` (Codex), `~/.claude/CLAUDE.md` (Claude Code), or `~/.config/opencode/AGENTS.md` (OpenCode). The routing frontmatter is stripped from the *generated copy*; the canonical source is never rewritten. Without `metadata.harness`, all globally enabled harnesses **with verified instructions adapters** are selected. Use `metadata.harness: none` to disable distribution. Existing unmanaged destination files are never overwritten, and changes to previously managed files require an unmodified ownership record. Other clients currently receive a warning rather than a guessed global instruction location.

## Supported harnesses

| Harness | User MCP configuration | User skills destination |
| --- | --- | --- |
| Claude Code (`claude-code`) | `~/.claude.json` | `~/.claude/skills/` |
| Codex (`codex`) | `~/.codex/config.toml` | `~/.agents/skills/` |
| Cursor (`cursor`) | `~/.cursor/mcp.json` | `~/.cursor/skills/` |
| OpenCode (`opencode`) | `~/.config/opencode/opencode.json` (or existing JSONC) | `~/.config/opencode/skills/` |
| Gemini CLI (`gemini`) | `~/.gemini/settings.json` | `~/.gemini/skills/` |
| Google Antigravity App (`antigravity`) | `~/.gemini/config/mcp_config.json` | `~/.gemini/config/skills/` |

The generated MCP entries run `agentcfg connect --server <name> --harness <client>` using the **installed executable's absolute path**. Clients retain distinct MCP names and their native config formats.

`claude-desktop` does **not** have an adapter yet; it is not interchangeable with Claude Code. Antigravity's CLI also discovers `~/.gemini/antigravity-cli/skills/`, but the App and CLI share the `~/.gemini/config/` personal layout; the current `antigravity` adapter targets that common layout. VS Code does not currently have a supported global native configuration adapter.

## Safety and current limits

- **No silent takeover:** unknown MCP names and unmanaged skill directories are preserved; collisions stop synchronization. Existing configurations are **not** imported automatically.
- **Native harness adapters:** synchronization writes supported user-level Claude Code, Codex, Cursor, OpenCode, Gemini and Antigravity configuration formats, retaining unrelated settings.
- **Not a sandbox:** installed MCP servers and plugin executables run with your account's permissions. Review untrusted packages and avoid committing credentials.
- Sharing is not isolation: the server's application state can be shared between harnesses. Use sharing: "isolated" for private state, roots, sampling, elicitation, or client-specific callbacks.
- **Protocol scope:** MCP `2026-07-28` request-scoped metadata, `server/discover`, stateless HTTP headers, and JSON/SSE responses use the official Go MCP SDK bridge alongside legacy 2025 handshakes. Modern protocol messages are routed per request; the broker does not merge modern clients into an initialized legacy stdio session. OAuth authorization is **explicit**, not an automatic login during MCP startup. Legacy standalone SSE transport and SSE cursor resumption are not implemented. Modern subscriptions and multi-round-trip tool results depend on the capabilities of both the upstream and the calling harness.
- **Not implemented:** bidirectional sync and automatic import of existing MCP definitions. Global instructions are supported only for verified user-scope Codex, Claude Code and OpenCode destinations; other instruction targets are not guessed.

## Development and documentation

```sh
git clone https://github.com/Shaar-games/agentcfg.git
cd agentcfg
mise trust
mise install
mise run check
mise run build
```

Go tests include a real subprocess integration fixture that verifies two harness proxies can reuse one upstream MCP process. Test results do **not** imply that every version of every harness has been end-to-end tested.

GitHub Actions tests Linux, Windows and macOS. Tagged releases build precompiled AMD64/ARM64 binaries for all three operating systems, publish SHA-256 checksums, and are consumed by the installation scripts. See [release process](docs/releases.md).

Explore the [CLI and setup guide](docs/cli.md), [shell completion setup](docs/completion.md), [configuration reference](docs/configuration.md), [synchronization behavior](docs/synchronization.md), [architecture](docs/architecture.md), [transports](docs/transports.md), [verification notes](docs/verification.md), and [roadmap](docs/roadmap.md).

<sub>Header and GitHub badges rendered with <a href="https://github.com/jal-co/shieldcn">shieldcn</a>. The README uses hosted SVGs; it does not require shieldcn at runtime.</sub>
