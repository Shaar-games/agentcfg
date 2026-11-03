// Package completion provides static, sourceable shell completions for agentcfg.
// Generation performs no I/O besides writing the requested output.
package completion

import (
	"fmt"
	"io"
	"strings"
)

var Shells = []string{"bash", "zsh", "fish", "powershell", "pwsh"}

var Commands = []string{
	"help", "version", "config", "init", "harness", "harnesses",
	"skill", "skills", "mcp", "mcps", "completion", "list", "validate",
	"sync", "connect", "auth", "update", "status", "stop", "daemon",
}

var options = map[string][]string{
	"init":       {"--harnesses", "--yes", "--plain", "--help"},
	"harness":    {"--plain", "--help"},
	"harnesses":  {"--plain", "--help"},
	"skill":      {"--plain", "--help"},
	"skills":     {"--plain", "--help"},
	"mcp":        {"--plain", "--help"},
	"mcps":       {"--plain", "--help"},
	"status":     {"--plain", "--json", "--help"},
	"list":       {"--plain", "--help"},
	"validate":   {"--plain", "--help"},
	"sync":       {"--apply", "--dry-run", "--yes", "--skill-mode", "--binary", "--plain", "--help"},
	"stop":       {"--yes", "--plain", "--help"},
	"connect":    {"--server", "--harness", "--help"},
	"auth":       {"login", "status", "logout"},
	"completion": {"bash", "zsh", "fish", "powershell", "pwsh"},
	"update":     {"--help"},
	"config":     {"--help"},
}

func Generate(shell string, out io.Writer) error {
	var body string
	switch strings.ToLower(strings.TrimSpace(shell)) {
	case "bash":
		body = bash()
	case "zsh":
		body = zsh()
	case "fish":
		body = fish()
	case "powershell", "pwsh":
		body = powershell()
	default:
		return fmt.Errorf("unknown shell %q; choose bash, zsh, fish, or powershell", shell)
	}
	_, err := io.WriteString(out, body)
	return err
}

func bash() string {
	var b strings.Builder
	b.WriteString("# Bash completion for agentcfg.\n_agentcfg_complete() {\n")
	b.WriteString("    local command cur\n    COMPREPLY=()\n    cur=\"$2\"\n")
	b.WriteString("    command=\"$")
	b.WriteString("{COMP_WORDS[1]}\"\n")
	b.WriteString("    if (( COMP_CWORD == 1 )); then\n        COMPREPLY=( $(compgen -W '")
	b.WriteString(strings.Join(Commands, " "))
	b.WriteString("' -- \"$cur\") )\n        return\n    fi\n    case \"$command\" in\n")
	for _, label := range []string{"init", "harness|harnesses", "skill|skills", "mcp|mcps", "status", "list", "validate", "sync", "stop", "connect", "auth", "completion", "update", "config"} {
		key := strings.Split(label, "|")[0]
		fmt.Fprintf(&b, "        %s) COMPREPLY=( $(compgen -W '%s' -- \"$cur\") ) ;;\n", label, strings.Join(options[key], " "))
	}
	b.WriteString("    esac\n}\ncomplete -F _agentcfg_complete agentcfg\n")
	return b.String()
}

func zsh() string {
	var b strings.Builder
	b.WriteString("#compdef agentcfg\n# Enable compinit before sourcing.\n_agentcfg() {\n    local -a suggestions\n    if (( CURRENT == 2 )); then\n        suggestions=(")
	b.WriteString(strings.Join(Commands, " "))
	b.WriteString(")\n    else\n        case \"$")
	b.WriteString("{words[2]}\" in\n")
	for _, label := range []string{"init", "harness|harnesses", "skill|skills", "mcp|mcps", "status", "list", "validate", "sync", "stop", "connect", "auth", "completion", "update", "config"} {
		key := strings.Split(label, "|")[0]
		fmt.Fprintf(&b, "            %s) suggestions=(%s) ;;\n", label, strings.Join(options[key], " "))
	}
	b.WriteString("        esac\n    fi\n    (( $#suggestions )) && compadd -- \"$")
	b.WriteString("{suggestions[@]}\"\n}\ncompdef _agentcfg agentcfg\n")
	return b.String()
}

func fish() string {
	var b strings.Builder
	b.WriteString("# Fish completion for agentcfg.\ncomplete -c agentcfg -f\n")
	fmt.Fprintf(&b, "complete -c agentcfg -n 'test (count (commandline -opc)) -eq 1' -a '%s'\n", strings.Join(Commands, " "))
	for _, label := range []string{"init", "harness harnesses", "skill skills", "mcp mcps", "status", "list", "validate", "sync", "stop", "connect", "auth", "completion", "update", "config"} {
		key := strings.Fields(label)[0]
		fmt.Fprintf(&b, "complete -c agentcfg -n '__fish_seen_subcommand_from %s' -a '%s'\n", label, strings.Join(options[key], " "))
	}
	return b.String()
}

func powershell() string {
	var b strings.Builder
	b.WriteString("# PowerShell 5.1+ / pwsh completion for agentcfg.\nRegister-ArgumentCompleter -Native -CommandName agentcfg -ScriptBlock {\n")
	b.WriteString("    param($wordToComplete, $commandAst, $cursorPosition)\n")
	fmt.Fprintf(&b, "    $commands = @('%s')\n    $parameters = @{\n", strings.Join(Commands, "','"))
	for _, key := range []string{"init", "harness", "harnesses", "skill", "skills", "mcp", "mcps", "status", "list", "validate", "sync", "stop", "connect", "auth", "completion", "update", "config"} {
		fmt.Fprintf(&b, "        '%s' = @('%s')\n", key, strings.Join(options[key], "','"))
	}
	b.WriteString("    }\n    $tokens = @($commandAst.CommandElements)\n    $candidates = $commands\n")
	b.WriteString("    $inArguments = ($tokens.Count -gt 2) -or ($tokens.Count -eq 2 -and $cursorPosition -gt $tokens[1].Extent.EndOffset)\n")
	b.WriteString("    if ($inArguments) {\n        $command = $tokens[1].Extent.Text.Trim([char]34, [char]39)\n")
	b.WriteString("        if ($parameters.ContainsKey($command)) { $candidates = $parameters[$command] }\n        else { $candidates = @() }\n    }\n")
	b.WriteString("    foreach ($candidate in $candidates) {\n        if ($candidate.StartsWith($wordToComplete, [StringComparison]::OrdinalIgnoreCase)) {\n")
	b.WriteString("            [System.Management.Automation.CompletionResult]::new($candidate, $candidate, 'ParameterValue', $candidate)\n")
	b.WriteString("        }\n    }\n}\n")
	return b.String()
}
