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

**All Huh navigation and action menus are Select fields.** Move with ↑/↓ and
press **Enter** to open the highlighted section, select an MCP or skill, or
confirm an action (including shared/isolated). No Space key is needed for a
single selection. **Only availability involving multiple harnesses, MCPs or
skills uses MultiSelect checkboxes**: Space checks/unchecks, Enter saves, and
Ctrl+C cancels. Current availability selections are prechecked. The interface
edits canonical settings; native harness files change only after an explicit
sync confirmation.

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
2. Shows Huh Select menus for navigation/actions and MultiSelect checkboxes
   only for multi-entry availability choices. The user chooses which
   harnesses, MCPs and skills to include in the canonical configuration.
3. Previews the proposed `~/.agentcfg/mcp.json`,
   `~/.agentcfg/agentcfg.json`, and skills.
4. Requests **explicit Huh confirmation before creating canonical sources**.
   Cancelling leaves them untouched.
5. At the end of initialization, previews the native harness synchronization
   plan and asks **whether to apply it**, using an explicit confirmation. The
   user may decline and keep only the canonical configuration.

The preview is available without writing anything:

```sh
agentcfg init --plain                  # Read-only discovery and preview
agentcfg init --harnesses codex,cursor # Limit the selected harnesses
```

`agentcfg init` handles both canonical creation and the **optional native
sync prompt** within that one interactive flow. It does not need an `--apply`
flag. Only exact native entries selected during discovery may be adopted after
their original file/tree fingerprints have been revalidated (compare-and-swap,
CAS). Supported JSON/JSONC MCP entries and byte-identical skill trees can be
adopted with explicit consent and backups; unrelated settings remain untouched.
**Native Codex TOML MCP adoption is refused**, including when the discovered
entry appears equivalent. Unexpected collisions, changed files or unproved
entries block the operation; no general force-overwrite mode exists.

For deliberate unattended canonical setup, `--yes` skips the interactive
creation confirmation **only**; it does not approve native synchronization:

```sh
agentcfg init --yes                   # Create canonical sources only; no native sync
```

Review the output or run `agentcfg init --plain` first. The optional final
native synchronization prompt exists **only in interactive initialization**.
`agentcfg init --yes` must never silently write native harness settings. To
apply native changes from a script, use a separate explicit command:

```sh
agentcfg init --yes
agentcfg sync --plain         # Review native changes without writing
agentcfg sync --apply --yes   # Explicit noninteractive native synchronization
```

The application still checks ownership, conflicts and fingerprints (CAS) and
creates backups as appropriate. Without the required confirmation, no writes
occur. **Existing canonical files and any unproved or modified native entries
must never be silently overwritten**. Resolve conflicts by inspecting the
paths and entries reported by the preview.

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
