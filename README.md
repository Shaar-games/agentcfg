<div align="center">

# agentcfg

**One MCP process. Multiple AI coding agents.**

Share local MCP servers, route skills and manage user-level harness configuration from one Go CLI.

[![CI](https://github.com/Shaar-games/agentcfg/actions/workflows/check.yml/badge.svg)](https://github.com/Shaar-games/agentcfg/actions/workflows/check.yml)
[![Release](https://img.shields.io/github/v/release/Shaar-games/agentcfg)](https://github.com/Shaar-games/agentcfg/releases/latest)

<img src="docs/demo/agentcfg-config.gif" width="850" alt="Animated agentcfg config menu selecting community-inspired example MCP servers" />

*Real `agentcfg config` recording, using safe, synthetic community-inspired fixtures. [VHS tape](docs/demo/agentcfg-config.tape).*

</div>

## What it does

- **One upstream, many clients** — a local broker shares MCP processes among Claude Code, Codex, Cursor, OpenCode and Gemini CLI. Start on demand, over private Unix sockets or Windows named pipes.
- **One configuration** — configure MCPs, skills and harness availability with [Huh](https://github.com/charmbracelet/huh), then preview and explicitly confirm native sync.
- **Safety first** — user-scope only; no silent replacement of native settings, with backups, ownership checks and conflict detection.
- **Portable workflows** — shell completion for Bash, Zsh, Fish and PowerShell, plus Agent Plugins support.

## Install

Download a precompiled release and verify its published SHA-256 checksum:

**Linux / macOS**

```sh
curl -fsSL https://raw.githubusercontent.com/Shaar-games/agentcfg/main/install.sh | bash
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/Shaar-games/agentcfg/main/install.ps1 | iex
```

The binary is installed in `~/.local/bin`. Installation does **not** modify any harness or MCP configuration. [Releases](https://github.com/Shaar-games/agentcfg/releases/latest) · [Installation details](docs/releases.md)

## Get started

```sh
agentcfg init           # Discover existing user-level harnesses, skills and MCPs
agentcfg config         # Configure with arrow keys and Enter
agentcfg list           # Browse configured items
agentcfg sync           # Preview changes to native harness configuration
```

`init` asks before creating canonical files, then offers a separate preview and confirmation before native sync. `init --plain` is read-only; `init --yes` creates **canonical files only**.

Use `agentcfg mcp`, `agentcfg skill` or `agentcfg harness` to jump directly to a section; plural aliases work too. `--plain` gives noninteractive output where supported.

## Browse the configuration

<img src="docs/demo/agentcfg-list.gif" width="850" alt="Animated agentcfg list browsing synthetic Vercel skills and Context7 and Playwright MCP entries" />

*Recorded with `agentcfg list`. The example names reference [Context7](https://github.com/upstash/context7), [Playwright MCP](https://github.com/microsoft/playwright-mcp) and [Vercel Agent Skills](https://github.com/vercel-labs/agent-skills); nothing is downloaded, executed or connected. [Tape and fixtures](docs/demo/).*

## How sharing works

```text
Claude Code ─┐
Codex       ├─ agentcfg connect ─ private IPC ─ broker ─ MCP server × 1
Cursor      ┘
```

Choose `isolated` instead of `shared` for MCP servers requiring separate sessions. Synchronization targets user-level harness files only and never overwrites unowned entries without explicit, verified adoption.

**Documentation:** [CLI](docs/cli.md) · [Configuration](docs/configuration.md) · [Synchronization](docs/synchronization.md) · [Architecture](docs/architecture.md) · [Completions](docs/completion.md)
