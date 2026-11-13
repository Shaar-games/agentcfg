# Command-line interface and initial setup

`agentcfg` without arguments (or `agentcfg help`) prints command help without
editing configuration. All canonical sources live under `~/.agentcfg/`.
The interactive configuration entry point is `agentcfg config`. If the canonical
settings and MCP files are missing, it offers to run initialization first.
Declining or cancelling that prompt writes nothing. If an older setup is only
partial and has never synchronized native files, the prompt offers to archive
its existing sources before initialization; it never merges or overwrites them.

## Direct configuration commands

The singular and plural forms are aliases. Each launches its section of the
same Huh interface:

| Commands | Operation | Read-only form |
| --- | --- | --- |
| `agentcfg harness`, `agentcfg harnesses` | Choose enabled user-level harnesses | `agentcfg harness --plain` |
| `agentcfg skill`, `agentcfg skills` | Choose which configured harnesses receive each skill | `agentcfg skills --plain` |
| `agentcfg mcp`, `agentcfg mcps` | Choose per-harness MCP availability and shared/isolated operation | `agentcfg mcps --plain` |
| `agentcfg config` | Open the compact configuration menu | Use the relevant category's `--plain` form |

Configuration and `agentcfg list` share the same six overview entries:
**Harnesses**, **MCP servers**, **Skills**, **Agent Plugins**, **Refresh** and
**Exit**. The four categories show aligned numeric counts only; Harnesses counts
the currently enabled harnesses. Refresh reloads the inventory without writing
configuration. Skill rows show enabled-harness counts; plugin rows show
MCP / skill counts, with warnings retained for invalid plugins.

Diagnostics and synchronization are not overview menu entries. Use
`agentcfg status`, `agentcfg validate` and `agentcfg sync` for those operations.

**All Huh navigation and action menus are Select fields.** Move with ↑/↓ and
press **Enter** to open the highlighted section, select an MCP or skill, or
confirm an action (including shared/isolated). No Space key is needed for a
single selection. **Only availability involving multiple harnesses, MCPs or
skills uses MultiSelect checkboxes**: Space checks/unchecks, Enter saves, and
Ctrl+C cancels. Current availability selections are prechecked. The interface
edits canonical settings; native harness files change only after an explicit
sync confirmation.

Menus show up to **ten option rows** before native Huh scrolling. Short lists
stay fully visible, and checkbox menus open at the first option while retaining
all prechecked values. Titles and wrapped descriptions have their own space.
**Back** (or **Exit** in the main menu) stays pinned below the scrollable options,
including while filtering. It occupies the tenth option row, leaving nine rows
for content. Short lists fill the gap with visual space. Arrow keys skip that space directly;
it adds no selectable entries or numbered accessible choices.
The empty space shrinks on smaller terminals so it does not cause scrolling.
Press Tab to focus the pinned action directly, including from a filter, then
Enter to activate it. Outside a filter, Esc also focuses that action.
The Harnesses checkbox screen also has a separate **Back** action: move below
the last checkbox or press Esc/Tab to focus it, then Enter to discard pending
changes and return. Enter on a checkbox still reviews the selection. Accessible
numbered mode accepts `b` followed by Enter to return without saving.
Long review notes also use a viewport of at most ten rows. Smaller terminals reduce that viewport while preserving native
scrolling and every selection. Only a terminal too small for even one content
row plus its help falls back to Huh's complete accessible list. Set `AGENTCFG_ACCESSIBLE=1` to request that mode explicitly.

Status columns align by terminal cell width, including wide Unicode labels.
Colors distinguish actions, information and warnings; `NO_COLOR` or
`CLICOLOR=0` disables navigation colors. Huh's footer supplies keyboard help.

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

## `agentcfg init`: import and migrate

`agentcfg init` reads the supported user-level MCP definitions and skills,
asks which AI harnesses to enable, then requests one confirmation. Approval
imports the complete portable inventory into `~/.agentcfg/`, replaces the
selected harnesses' MCP entries with `agentcfg connect` broker proxies, and
copies the canonical skills into those harnesses' own directories.

Native MCP provenance does not restrict availability: every imported MCP is
available to all selected harnesses. Explicit skill `metadata.harness` filters
remain effective. Imported skills are removed from their old native locations,
including shared/legacy roots and unselected harnesses; selected destinations
are recreated as physical copies. Neither MCP processes nor skills are executed
during initialization. Unrelated native settings are preserved.

```sh
agentcfg init --plain                     # Read-only discovery and full details
agentcfg init                            # Choose harnesses and confirm once
agentcfg init --harnesses codex,cursor     # Set the destination selection
agentcfg init --harnesses codex,cursor --yes # Approve the complete migration
```

`--yes` authorizes both canonical creation and native migration. There is no
optional synchronization step after successful initialization. JSON/JSONC and
Codex TOML MCP entries are adopted only with revalidated scanner proofs.
Original files and skill directories are backed up for `agentcfg restore`.

The app-owned Codex desktop `node_repl` service keeps its native configuration
and private session pipes. It is listed under `Kept native` in `init --plain`,
rather than being treated as a failed user MCP import. Ordinary user MCPs named
`node_repl` are not excluded. Unsupported options are reported by MCP name and
option name, without printing their values.

Init preserves an explicitly configured `cwd` and does not add the home directory
when it is absent. Without an explicit `cwd`, a stdio MCP uses the connecting
harness's working directory. Sharing applies to clients with the same effective
working directory. After upgrading from an older broker protocol, run
`agentcfg stop` before reconnecting; status and stop remain compatible with the
older broker.

Unsupported active MCP definitions, unreadable/nonportable skills and ambiguous
names block initialization before canonical publication; `init --plain` shows
the source details. Init must not report success with a partial inventory.
A later migration failure attempts to restore the native baseline and archives
the imported canonical sources, so a failed attempt can be reviewed and retried.

After success, only `~/.agentcfg/` supplies upstream definitions and skill
content. Use `agentcfg config` to edit availability, then `agentcfg sync --plain`
and `agentcfg sync --apply` for subsequent changes. Skills are copied by default;
`sync --skill-mode auto` or `symlink` remains an explicit alternative. Native
manual edits remain conflicts, rather than being silently overwritten.

For an earlier canonical-only initialization, run `agentcfg restore` before
initializing again. Restoration preserves the old canonical data in an archive.

## Shell completion

`agentcfg completion <shell>` prints a completion script for Bash, Zsh, Fish,
PowerShell 5.1, or PowerShell 7. The accepted shell IDs are `bash`, `zsh`,
`fish`, `powershell` and `pwsh`; the last two generate the same script.
See [completion.md](completion.md) for commands to load it now or install it
persistently.

## Other useful commands

Connections started by a harness through `agentcfg connect` open the browser
when an HTTP MCP requires OAuth. A compatible URL-only server uses automatic
discovery; no preliminary terminal login or configuration rewrite is required.
`agentcfg auth login <server>` remains available for an explicit login or a
manual callback. `agentcfg auth status <server>` and `agentcfg auth logout <server>`
use the same credential identity as automatic connections. See
[transports.md](transports.md) for callback and secure-storage requirements.

`agentcfg status` inspects the broker without starting it;
`agentcfg stop` stops a running broker. `agentcfg update` updates the
executable, while `agentcfg version` prints the current version.

See the [canonical configuration](configuration.md) and
[synchronization safety](synchronization.md) references for data formats,
ownership tracking and conflict handling.

## `agentcfg restore`: return to the pre-init state

```sh
agentcfg restore --plain  # Read-only recovery preview
agentcfg restore          # Review and confirm in an interactive terminal
agentcfg restore --yes    # Explicitly approve recovery without prompts
```

New initialization writes a recovery marker alongside the canonical files.
The first native synchronization captures the files and skill trees it is
about to replace, before changing any native destination. Restoration reuses
sync's preimage checks, backups and rollback. It restores existing native MCP
configuration byte-for-byte, restores original skill directories, and removes
native destinations created by that initial synchronization.

The entire `~/.agentcfg/` directory, including later canonical edits, original
backups and recovery data, moves to `~/.agentcfg-restored-<timestamp>/`.
No canonical content is deleted. After recovery, `agentcfg init` can run again.
Native files must still match their recorded initialization output (or already
be back at their original state). A manual native edit or a later sync that
changed that output blocks recovery before any native write; it is never
silently overwritten.

A partial or canonical-only setup made by an older version can also be
archived if there is no native sync manifest or backup history. Older setups
that have already synchronized native files but have no recovery marker are
refused: the command cannot prove their pre-init state. Their existing backups
remain in `~/.agentcfg/backups/` for deliberate manual recovery.
