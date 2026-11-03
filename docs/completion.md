# Shell completion

`agentcfg completion <shell>` writes a **sourceable completion script to
standard output**. Supported identifiers are `bash`, `zsh`, `fish`,
`powershell` and `pwsh`. Both PowerShell identifiers generate the same
completion registration code, compatible with Windows PowerShell 5.1 and
PowerShell 7.

Generation is local and does not inspect, create or alter configuration.
The completion covers the known commands and options; it does not
connect to MCP servers to discover dynamic names.

Make sure the installed `agentcfg` executable is on your `PATH` before
loading its completion. The commands below are **instructions to run in
your own shell**, not steps performed by the installer.

## Bash

Load completion in the current interactive Bash session:

```bash
source <(agentcfg completion bash)
```

For future interactive sessions, add that line **once** to `~/.bashrc`.
Alternatively, on systems with `bash-completion` installed and enabled,
install the generated script in the per-user completion directory:

```bash
mkdir -p ~/.local/share/bash-completion/completions
agentcfg completion bash > ~/.local/share/bash-completion/completions/agentcfg
```

Open a new Bash session after installing the script.

## Zsh

Initialize the Zsh completion system before loading the generated script:

```zsh
autoload -Uz compinit
compinit
source <(agentcfg completion zsh)
```

To enable completion every time, add `source <(agentcfg completion zsh)`
**after the existing `compinit` line** in `~/.zshrc`. If your Zsh
startup file does not initialize completion, add the three lines shown
above. Keep a single copy of the registration so the function is not
loaded repeatedly.

## Fish

Load completion in the current Fish session:

```fish
agentcfg completion fish | source
```

For persistent completion, generate a user completion file (Fish loads
files in this directory automatically):

```fish
mkdir -p ~/.config/fish/completions
agentcfg completion fish > ~/.config/fish/completions/agentcfg.fish
```

Open a new Fish session after installing the file.

## Windows PowerShell 5.1

Load completion in the current Windows PowerShell session:

```powershell
agentcfg completion powershell | Out-String | Invoke-Expression
```

For future sessions, add the following line **once** to your Windows
PowerShell `$PROFILE` file:

```powershell
agentcfg completion powershell | Out-String | Invoke-Expression
```

If `$PROFILE` does not exist, create its containing directory and an empty
profile file first:

```powershell
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $PROFILE) | Out-Null
if (-not (Test-Path -LiteralPath $PROFILE)) { New-Item -ItemType File -Path $PROFILE | Out-Null }
```

Open `$PROFILE` in your editor, add the registration line, and restart
PowerShell. An execution policy that forbids loading a profile may need
to be addressed through your normal PowerShell policy settings.

## PowerShell 7 (`pwsh`)

Load the same completion in a running PowerShell 7 session:

```powershell
agentcfg completion pwsh | Out-String | Invoke-Expression
```

For persistent setup, add that line once to PowerShell 7's own
`$PROFILE`. PowerShell 7 and Windows PowerShell 5.1 may use different
profile paths; configure them separately if you use both.

## Checking the result

Start typing `agentcfg ` and press Tab. The completion offers commands
such as `config`, `init`, `harness`/`harnesses`,
`skill`/`skills` and `mcp`/`mcps`, as well as supported flags
after a subcommand.

If Tab does not suggest completions, check that `agentcfg` resolves to the
installed executable, the generated script has been loaded in the active
shell and (for Zsh) `compinit` ran first.
