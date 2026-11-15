<div align="center">

<img src="https://shieldcn.dev/header/glow.svg?title=agentcfg&amp;subtitle=One+MCP+process.+Every+AI+agent.&amp;logo=go&amp;theme=blue&amp;size=banner" alt="agentcfg — shared MCP processes and skills for AI coding agents" width="750" />

**One MCP process. Multiple AI coding agents.**

Share local MCP servers, route skills and manage user-level harness configuration from one Go CLI.

<a href="https://github.com/Shaar-games/agentcfg/actions/workflows/check.yml"><img alt="GitHub CI" src="https://shieldcn.dev/github/ci/Shaar-games/agentcfg.svg?variant=outline&amp;size=sm" /></a>
<a href="https://github.com/Shaar-games/agentcfg"><img alt="GitHub stars" src="https://shieldcn.dev/github/stars/Shaar-games/agentcfg.svg?variant=outline&amp;size=sm" /></a>
<a href="https://github.com/Shaar-games/agentcfg/issues"><img alt="GitHub issues" src="https://shieldcn.dev/github/issues/Shaar-games/agentcfg.svg?variant=outline&amp;size=sm" /></a>
<img alt="Go 1.26" src="https://shieldcn.dev/badge/Go-1.26-blue.svg?variant=outline&amp;size=sm&amp;logo=go" />
<a href="https://github.com/Shaar-games/agentcfg/releases/latest"><img alt="Latest release" src="https://shieldcn.dev/github/release/Shaar-games/agentcfg.svg?variant=outline&amp;size=sm" /></a>

<img src="docs/demo/agentcfg-config.gif" width="850" alt="Animated agentcfg config menu selecting community-inspired example MCP servers" />

</div>

## What it does

- **One upstream, many clients** — a local broker shares MCP processes among Claude Code, Codex, Cursor, OpenCode and Gemini CLI. Start on demand, over private Unix sockets or Windows named pipes.
- **One configuration** — configure MCPs, skills and harness availability with [Huh](https://github.com/charmbracelet/huh), then preview and explicitly confirm native sync.
- **Native HTTP OAuth** — opt in to a loopback gateway so compatible MCP clients can authorize HTTP servers through their normal OAuth flow. Local access tokens stay separate from upstream credentials.
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

`init` asks once before importing sources and migrating the selected harnesses. After success, `~/.agentcfg` is the single source of truth: MCPs use broker proxies by default and skills are copied from the canonical inventory. `init --plain` is read-only; `init --yes` approves the complete migration.

Opening `agentcfg config` before setup offers to run `init`. Use `agentcfg restore --plain` to preview recovery to the pre-init state, then `agentcfg restore` to confirm. Native files are checked before restoration; canonical sources and backups are archived. [Recovery details](docs/cli.md#agentcfg-restore-return-to-the-pre-init-state).

Use `agentcfg mcp`, `agentcfg skill` or `agentcfg harness` to jump directly to a section; plural aliases work too. `--plain` gives noninteractive output where supported.

For a client that supports OAuth over Streamable HTTP, add
`"gateway": {"enabled": true, "harnesses": ["cursor"]}` to your existing
`~/.agentcfg/agentcfg.json` to select Cursor, then:

```sh
agentcfg gateway start       # Start the loopback HTTP service explicitly
agentcfg sync --plain        # Review HTTP URL projections
agentcfg sync --apply        # Confirm the migration of managed entries
agentcfg gateway status      # Inspect the service without starting it
```

The gateway defaults to `http://127.0.0.1:43877` and requires local OAuth even for
a public upstream. The client handles authorization as an HTTP resource;
agentcfg adds no authentication tool to the MCP tool list. The client's support
and interface determine how sign-in appears. HTTPS is optional for clients that
require it. A configured URL does not start the service: use `gateway start`, or
configure `gateway run` in your own user service or login startup.

`gateway start` initializes an encrypted routing key before sync can generate
the URLs. These URLs contain private per-resource capabilities: keep them out of
public logs and shared configuration. Sync checks native files and recovery copies
that contain these URLs, and reports unsafe permissions without changing them
automatically.

An omitted or empty `gateway.harnesses` list selects all enabled harnesses. An
explicit list lets other clients, such as OpenCode, keep their stdio entries.
For a client without native HTTP OAuth support, keep `agentcfg connect` and run
`agentcfg auth login <server>`, then refresh or reconnect the client.
`--no-browser` selects manual URL/callback entry. Automatic browser opening from
stdio connections stays disabled unless `oauth.autoOpenBrowser` is `true`.
The gateway is disabled by default; upgrading does not migrate existing native
entries. See [gateway setup](docs/cli.md#native-oauth-gateway) and
[configuration](docs/configuration.md#native-oauth-gateway).

## Browse the configuration

<img src="docs/demo/agentcfg-list.gif" width="850" alt="Animated agentcfg list browsing synthetic Vercel skills and Context7 and Playwright MCP entries" />

## How sharing works

| Connection | Runtime behavior |
| --- | --- |
| `agentcfg connect` over stdio | Private IPC connects clients to the broker; `shared` reuses an upstream, while `isolated` gives each client its own connection. |
| HTTP URL projected by the enabled gateway | The client speaks HTTP directly to the loopback gateway; upstream HTTP sessions stay separate instead of using broker multiplexing. |

Synchronization targets user-level harness files only and never overwrites unowned entries without explicit, verified adoption.

**Documentation:** [CLI](docs/cli.md) · [Configuration](docs/configuration.md) · [Synchronization](docs/synchronization.md) · [Architecture](docs/architecture.md) · [Completions](docs/completion.md)
