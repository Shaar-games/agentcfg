# Migrate to agentcfg — instructions for AI coding agents

This file is an **operational migration playbook** for AI coding agents helping a user move existing MCP server definitions and Agent Skills into [agentcfg](README.md). Read it **before changing anything**.

## Mission and non-negotiable boundaries

**Goal:** establish `~/.agentcfg/` as the single user-level source of MCP servers, skills, and portable Agent Plugins, then synchronize the native configurations of supported harnesses without losing the user's existing data.

1. **Do not inspect, create, or modify project-level configuration.** No changes to the current project's `.agents/`, `.claude/`, `.cursor/`, `AGENTS.md`, `CLAUDE.md`, `.mcp.json`, or other project files. Operate on the user's home directory only.
2. **Never silently replace an existing MCP entry or skill.** Back up first. The current synchronizer intentionally refuses to adopt an unowned entry with the same name.
3. **Do not run unknown MCP commands just to discover what they do.** Reading configuration is not authorization to execute servers, download packages, or expose credentials.
4. **Keep credentials private.** Do not paste environment variables, tokens, credential files, or header values into chat reports, public issues, or version control.
5. **Request authorization for destructive changes.** Present the exact user-level files/entries that would be replaced or removed, along with backups and a rollback plan. Only apply approved changes.
6. **Never infer missing configuration.** If a client-specific value cannot be translated, report it rather than guessing or silently discarding it.

## Supported product behavior

| Concern | Current implementation |
| --- | --- |
| Canonical root | `~/.agentcfg/`, always user-level |
| CLI | `agentcfg` (help), `agentcfg config` (Huh configuration), `agentcfg harnesses` (direct Huh multi-select), `migrate` / `migrate --apply`, `init`, `validate`, `list`, `update`, `sync`, `sync --apply`, `status`, `stop` |
| Configured harnesses | `claude-code`, `codex`, `cursor`, `opencode`, `gemini`, `antigravity` |
| Unsupported in global sync | `claude-desktop`; VS Code's adapter is project-only |
| MCP execution | Local stdio proxies, one shared upstream or an isolated backend |
| Skills | Directory links (copy fallback) or copies to native user-level directories |
| Existing entries | **Not automatically imported or adopted** |
| Bidirectional sync | Not implemented |
| Global instruction file | `~/.agentcfg/AGENTS.md` distributes user-level instructions to Codex, Claude Code and OpenCode when selected |

There is **no `--root` CLI flag** and no project synchronization option. Do not use commands from older agentcfg documentation that include them. Running `agentcfg` without arguments only displays help; it does not launch a form or make changes.

## Renaming an existing agentmux installation

The renamed application uses `~/.agentcfg/`, while the former **agentmux** canonical directory and executable are preserved by default. Before running `agentcfg init` on a home that already contains legacy canonical sources, examine the built-in migration preview:

```sh
agentcfg migrate           # Read-only preview from ~/.agentmux/ to ~/.agentcfg/
agentcfg migrate --apply   # Explicitly apply the reviewed source migration
agentcfg validate          # Check the resulting canonical configuration
agentcfg config            # Review selected harnesses and skill availability in Huh
```

Treat proposed conflicts or existing destination files as material: inspect the proposed changes and backups before applying. Never delete the old directory or binary as part of this name change. The `migrate` command addresses the **legacy canonical sources**, not client-specific MCP definitions already stored in native Claude, Codex, Cursor or other harness files. It does not execute `sync --apply` or authorize overwriting native configuration; steps 1–7 below remain necessary for that separate operation. On a new installation without `~/.agentmux/`, use `agentcfg init` to create missing sources.

## Moving from the original project-local source layout

Early development revisions used **`<project>/.agents/`**, followed by
**`<project>/.agentcfg/`**, before the public interface became exclusively
user-level. The current program neither loads nor migrates these project
sources. The repository itself does not ship a live `.agentcfg/` directory.

**Never copy an old project manifest or delete old managed entries
automatically.** If the user explicitly authorizes a migration of an archived
project-local source, inspect only the specified archive, **read-only**; back it
up; then *propose* a mapping of each MCP, plugin, skill and instruction into
`~/.agentcfg/`. Check old relative `command`, `cwd`, `args` and script
paths: they may have been resolved from the project rather than the home and
must not be carried over blindly. Ask approval before writing the new
user-level sources. Do not mistake native `~/.agents/skills/` (used by Codex)
for the old canonical source.

The old project-scoped sync ownership file is **not interchangeable** with
`~/.agentcfg/sync-user.json`. Do not rename or copy it into the new home.
Individually reconcile any old native generated MCP entries against their
actual contents and backups before running a user-scope sync. No automated
rollback of the old project layout is implemented.

## Step 1 — Inventory the current user setup

Inspect **only** supported user-level files, if present:

| Harness | Native MCP config | Native skills |
| --- | --- | --- |
| Claude Code | `~/.claude.json` | `~/.claude/skills/` |
| Codex | `~/.codex/config.toml` | `~/.agents/skills/` |
| Cursor | `~/.cursor/mcp.json` | `~/.cursor/skills/` |
| OpenCode | `~/.config/opencode/opencode.json` (or existing `.jsonc`) | `~/.config/opencode/skills/` |
| Gemini CLI | `~/.gemini/settings.json` | `~/.gemini/skills/` |
| Google Antigravity App / CLI | `~/.gemini/config/mcp_config.json` | `~/.gemini/config/skills/` (shared personal layout) |

Find the user's own MCP server names, transport types, executable names, arguments, working directories, environment-variable **names**, remote URLs, and any client-specific settings. Treat private values as opaque; preserve them securely without displaying them.

For each MCP, record which harnesses currently use it and whether those definitions are truly equivalent. **A matching name is not proof of an identical MCP.** Different commands, credentials, namespaces, state, or working directories may require separate names or `sharing: "isolated"`.

For skills, inventory `SKILL.md` plus the whole skill tree, not just the Markdown file. Note duplicate names, plugin-owned skills, and any native discovery rules that bypass generated links.

## Step 2 — Create recoverable backups

- Before editing a native user configuration, make a timestamped **private** backup outside any Git repository. Preserve original bytes and access permissions wherever possible.
- Back up `~/.agentcfg/` if it already exists; **merge with existing canonical sources**, never replace them wholesale.
- Record the original paths, which entries belong to each client, and how to restore them.
- Do not put secrets, private skill files, or backups inside the agentcfg source repository.
- Keep the backups until the user verifies each harness and approves cleanup.

The agentcfg synchronizer also creates backups of changes it owns, but **that is not a substitute for backing up the original configurations during migration**.

## Step 3 — Prepare agentcfg

If not installed, review the [installation scripts](README.md#installation) with the user. Bash or PowerShell downloads verified, precompiled GitHub Release binaries; neither a Go toolchain nor Git nor mise is needed for installation. Installation is separate from configuration migration.

```sh
agentcfg version
agentcfg init
agentcfg validate
```

`init` creates missing empty canonical files and directories without overwriting existing ones. Edit only:

```text
~/.agentcfg/
├── mcp.json
├── agentcfg.json
├── skills/
└── plugins/
```

The source can also contain `AGENTS.md`; `metadata.harness` selects which verified user-level adapters receive it. agentcfg strips the routing frontmatter from generated native copies and preserves the canonical original. This feature supports Codex (`~/.codex/AGENTS.md`), Claude Code (`~/.claude/CLAUDE.md`) and OpenCode (`~/.config/opencode/AGENTS.md`) only, and rejects collisions with existing unmanaged instruction files. Do not assume other clients are supported.

## Step 4 — Translate MCP definitions

Create/update the canonical `mcpServers` object in `~/.agentcfg/mcp.json`. The following is **illustrative only**, not a request to add these servers automatically:

```json
{
  "mcpServers": {
    "my-local-mcp": {
      "type": "stdio",
      "command": "node",
      "args": ["/absolute/path/to/server.js"],
      "sharing": "shared",
      "harnesses": ["claude-code", "codex", "cursor"]
    },
    "my-remote-mcp": {
      "type": "streamable-http",
      "url": "https://example.com/mcp"
    }
  }
}
```

- Preserve actual executable names, arguments, environment settings, URLs and authorization requirements, including the difference between stdio and Streamable HTTP.
- Use `harnesses` to include only the clients that originally had (or have been approved to receive) a server. Absence means all globally selected clients.
- Use `sharing: "shared"` only when a server is safe to share. Use `"isolated"` for private browser sessions, client-specific state, callbacks, roots, sampling, or elicitation.
- On Windows, prefer invoking the underlying executable directly if a batch wrapper cannot safely pass the existing arguments.
- Do not automatically convert legacy HTTP+SSE configurations: the separate-endpoint transport is unsupported. For OAuth-protected Streamable HTTP MCPs, preserve the intended resource, issuer, scopes and client registration, then configure `oauth` and perform a separate explicit `agentcfg auth login <server>` with consent. Do not copy existing client access or refresh tokens into plaintext files.
- If distinct MCPs collide on the same name, choose unique canonical names **with the user's approval**.

For standard Agent Plugins, keep each package under `~/.agentcfg/plugins/<plugin>/` with its `plugin.json`, standard `mcp.json`, and optional `skills/`. **Do not add `harnesses` or `sharing` to a portable plugin's `mcp.json`.** Use qualified names in `serverOverrides` of `~/.agentcfg/agentcfg.json` instead.

## Step 5 — Translate skills and select harnesses

Store standalone skills as `~/.agentcfg/skills/<skill>/SKILL.md`, keeping their associated scripts and references. Portable plugin skills stay under their plugin's `skills/` directory. Verify the frontmatter name matches the skill directory.

For a subset of harnesses, add a **comma-separated string** in the skill's standard metadata map:

```yaml
---
name: code-review
description: Review code and report actionable findings.
metadata:
  harness: codex,claude-code,cursor
---
```

An omitted `metadata.harness` means all selected harnesses; `metadata.harness: none` explicitly selects no harness. A YAML list is not valid for this field. **Do not use `skillOverrides`** in `agentcfg.json`.

Set the globally selected harnesses in `~/.agentcfg/agentcfg.json`:

```json
{
  "harnesses": ["claude-code", "codex", "cursor", "opencode", "gemini"],
  "runtime": {
    "idleTimeout": "5m",
    "requestTimeout": "5m"
  }
}
```

The user may choose a smaller list. Skill filters control agentcfg's generated destinations, **not independent native discovery**; for example, another harness may scan `~/.agents/skills/` even when that directory is installed for Codex. Report this limitation rather than claiming strict isolation.

## Step 6 — Validate, preview and resolve conflicts

```sh
agentcfg validate
agentcfg list
agentcfg sync
```

`agentcfg sync` is read-only. Examine its proposed changes and warnings. It may fail on a name collision between an existing user MCP entry and an agentcfg-generated entry.

**Do not use a force flag, delete the synchronization manifest, or overwrite native files to hide a conflict.** There is no automatic adoption operation.

If a conflicting native entry must be migrated:

1. Verify that the same behavior has been represented in `~/.agentcfg/mcp.json` or a portable plugin.
2. Show the user **which exact entry in which user-level config file** would need to be removed, plus the before/after diff and backup location.
3. Obtain approval. Remove **only** that approved old entry while preserving the rest of the native config (and its JSON/JSONC/TOML structure).
4. Re-run `agentcfg sync`. If another conflict remains, stop and resolve it separately.

An unmanaged skill collision must likewise be resolved explicitly; do not silently remove its existing directory. If an existing harness configuration is managed by another tool, document the ownership conflict instead of fighting that tool.

## Step 7 — Apply and verify

After the user reviews the changes (`agentcfg config` provides the harness, skill, MCP sharing, and confirmed synchronization screens; `agentcfg harnesses` opens just the Huh harness checklist):

```sh
agentcfg sync --apply
agentcfg sync
agentcfg status
```

Verify that the second `sync` has no unexpected changes. `status` may say the broker is stopped; it does **not** start MCP servers itself. An actual harness connection starts the broker on demand.

Check each selected harness as available:

- Its MCP names appear individually, with the expected `agentcfg connect` proxy commands.
- The correct skills are discoverable, without unintended duplicates.
- An MCP can initialize and perform a harmless read-only operation where appropriate.
- Two harnesses using the **same shared** MCP reuse a single upstream process (the broker's status and process IDs help verify this).
- An `isolated` MCP gets separate upstream connections where required.

Do not claim full application compatibility if the harness application was not launched or tested.

## Rollback and handoff

If the migration fails, stop newly generated activity when safe, preserve diagnostic information **without secrets**, and restore the **specific approved native files** from pre-migration backups. Do not delete `~/.agentcfg/sync-user.json` or generated files indiscriminately: that manifest tracks ownership, and deleting it can make managed entries appear unowned and cause future conflicts. Reconcile the state explicitly before attempting a new sync.

Provide the user a concise final report covering **migrated MCPs, migrated skills, selected harnesses, shared versus isolated choices, changed user-level paths, backup locations, commands actually run, verified behavior, and unresolved blockers**. Distinguish planned steps from performed steps.

## Ready-to-use request

> Read https://github.com/Shaar-games/agentcfg/blob/main/MIGRATION_FOR_AGENTS.md and migrate my existing **user-level** MCP servers and skills into `~/.agentcfg/`. Do not touch the current project. Inventory and back up existing native configurations, preserve secrets, show a concrete diff for every conflicting entry, and request my approval before deleting/replacing an old entry or running `agentcfg sync --apply`. Validate and report exactly what you verified.

See [README.md](README.md), [configuration](docs/configuration.md), and [synchronization](docs/synchronization.md) for details.
