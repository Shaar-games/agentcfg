# Command-line interface and initial setup

`agentcfg` without arguments (or `agentcfg help`) prints command help without
editing configuration. All canonical sources live under `~/.agentcfg/`.
The interactive configuration entry point is `agentcfg config`.

## Direct configuration commands

The singular and plural forms are aliases. Each launches its section of the
same Huh interface:

| Commands | Operation | Read-only form |
| --- | --- | --- |
| `agentcfg harness`, `agentcfg harnesses` | Choose enabled user-level harnesses | `agentcfg harness --plain` |
| `agentcfg skill`, `agentcfg skills` | Choose which configured harnesses receive each skill | `agentcfg skills --plain` |
| `agentcfg mcp`, `agentcfg mcps` | Choose per-harness MCP availability and shared/isolated operation | `agentcfg mcps --plain` |
| `agentcfg config` | Open the complete Huh navigation menu | Use the relevant category's `--plain` form |

Huh uses the same checklist navigation in its sections: ↑/↓ moves, Space
toggles a choice, Enter confirms, and Ctrl+C cancels. Harness and skill
availability are multi-select; the current effective selections are prechecked.
The interface edits the relevant canonical settings or availability rules, and
native harness configuration is changed only through a confirmed sync.

`--plain` on a category command prints its effective settings without opening
Huh, editing sources or starting MCP processes. It is useful in scripts:

```sh
agentcfg harnesses --plain
agentcfg skill --plain
agentcfg mcps --plain
agentcfg list --plain
agentcfg validate --plain
agentcfg sync --plain         # Read-only preview of native changes
agentcfg status --json        # Machine-readable broker state
```

`--plain` controls presentation on supported commands. It does not implicitly
approve a write: `sync --apply` and any confirmation bypass such as `--yes`
are separate, explicit operations. `agentcfg config` is the interactive menu,
so use an individual category with `--plain` in a noninteractive workflow.

## `agentcfg init`: discover, preview, confirm

Initial setup inspects **existing user-level native MCP definitions and skill
directories** of supported, installed harnesses. Discovery is limited to the
user's home; it does not inspect open projects, start MCP servers, run skills
or silently edit native configuration files.

On an interactive terminal, `agentcfg init`:

1. Identifies installed harnesses and reads their supported native MCP and
   skill sources. It presents discovered candidates and any duplicate names
   or configurations that cannot be safely resolved.
2. Shows Huh selection lists so the user can choose harnesses and the entries
   to include in the canonical configuration.
3. Previews the proposed `~/.agentcfg/mcp.json`,
   `~/.agentcfg/agentcfg.json`, and skills.
4. Requests **explicit Huh confirmation before creating canonical sources**.
   Cancelling does not authorize any change.

The preview is available without writing anything:

```sh
agentcfg init --plain                  # Read-only discovery and preview
agentcfg init --harnesses codex,cursor # Limit the selected harnesses
```

To create the canonical sources and also synchronize supported native harness
settings, use:

```sh
agentcfg init --apply
```

This runs the discovery and confirmation flow, shows a separate native sync
plan and asks for **a second Huh confirmation** before applying native changes.
Only the exact native entries selected during discovery may be adopted, after
their original file/tree fingerprints have been revalidated. JSON/JSONC MCP
entries and byte-identical skill trees can be adopted with this explicit
consent; unrelated settings remain untouched. Native Codex TOML MCP adoption
is deliberately refused until it can be done without risking unrelated tables.
Unexpected name collisions, changed files and unproved entries still block
the operation; no general force-overwrite mode exists.

For deliberate unattended use, `--yes` is the explicit alternative to Huh
confirmation:

```sh
agentcfg init --yes                   # Confirm canonical creation in a script
agentcfg init --apply --yes           # Confirm creation and native sync
```

Review the output or run `agentcfg init --plain` first. Regardless of the
mode, **existing canonical files and any unproved or modified native entries
must never be silently overwritten**. Resolve
conflicts by inspecting the paths and entries reported by the preview.

After configuration, `agentcfg validate` checks canonical sources, and
`agentcfg sync` produces an independent read-only preview. For ongoing
changes outside initialization, use `agentcfg sync --apply` only after
reviewing that plan.

## Shell completion

`agentcfg completion <shell>` prints a completion script for Bash, Zsh, Fish,
PowerShell 5.1, or PowerShell 7. The accepted shell IDs are `bash`, `zsh`,
`fish`, `powershell` and `pwsh`; the last two generate the same script.
See [completion.md](completion.md) for commands to load it now or install it
persistently.

## Other useful commands

`agentcfg auth login <server>` explicitly starts an OAuth authorization
flow. `agentcfg status` inspects the broker without starting it;
`agentcfg stop` stops a running broker. `agentcfg update` updates the
executable, while `agentcfg version` prints the current version.

See the [canonical configuration](configuration.md) and
[synchronization safety](synchronization.md) references for data formats,
ownership tracking and conflict handling.
