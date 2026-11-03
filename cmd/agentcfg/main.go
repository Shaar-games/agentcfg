package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"agentcfg/internal/broker"
	"agentcfg/internal/completion"
	"agentcfg/internal/config"
	"agentcfg/internal/syncer"
	"agentcfg/internal/tui"
	"agentcfg/internal/updater"
)

// Set at release build time: -ldflags '-X main.version=vX.Y.Z'.
var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agentcfg:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, input io.Reader, output, diagnostics io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(output, `agentcfg — shared MCP processes and declarative skills

Usage: agentcfg [command] [options]

  (no command) / help   Display this help (no configuration changes)
  config                Open complete user-level Huh configuration
  harness, harnesses    Configure enabled AI harnesses (Huh checkboxes)
  skill, skills         Configure skill availability by harness
  mcp, mcps             Configure MCP availability and sharing by harness
  completion <shell>    Print Bash, Zsh, Fish or PowerShell completion

  init       Discover native harness MCPs/skills; confirm canonical setup
  list       Show discovered MCP servers and skills
  validate   Validate canonical configuration and plugin discovery
  sync       Preview native harness changes; --apply writes them
  connect    Stdio proxy for one MCP; starts the private broker on demand
  auth       Explicit OAuth login, status, or logout for a configured MCP
  update     Self-update this executable to the latest published release
  status     Show the broker and upstream process IDs (does not start it)
  stop       Stop this root's broker and its managed MCP children
  daemon     Run the local IPC broker in the foreground
  version    Print version

All commands use the current user's ~/.agentcfg configuration.
Human-facing CLI commands use Huh forms in a TTY; --plain keeps script output.
Use agentcfg <command> --help for command-specific options.
No persistent web server or TCP listener is used. OAuth login may temporarily
listen on a loopback callback address when explicitly invoked.
Bidirectional sync is not implemented.`)
		return err
	}
	if args[0] == "version" {
		_, err := fmt.Fprintln(output, version)
		return err
	}
	if args[0] == "completion" {
		if len(args) != 2 || args[1] == "--help" || args[1] == "-h" {
			_, err := fmt.Fprintln(output, "Usage: agentcfg completion <bash|zsh|fish|powershell>\nPrint a sourceable completion script to stdout.")
			return err
		}
		return completion.Generate(args[1], output)
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	root := &home
	switch args[0] {
	case "update":
		if err := parse(flags, args[1:]); err != nil {
			return helpResult(err)
		}
		result, err := updater.Update(ctx, updater.Options{CurrentVersion: version})
		if err != nil {
			return err
		}
		if result.AlreadyCurrent {
			_, err = fmt.Fprintf(output, "No update needed (installed: %s, latest stable release: %s).\n", version, result.Version)
		} else {
			_, err = fmt.Fprintf(output, "Updated agentcfg to %s: %s\n", result.Version, result.Path)
		}
		if result.BackupPath != "" {
			fmt.Fprintf(diagnostics, "Previous executable remains in use; remove %s after exiting agentcfg.\n", result.BackupPath)
		}
		if err == nil && !result.AlreadyCurrent {
			_, err = fmt.Fprintln(output, "Existing broker processes keep running until stopped; restart them when convenient with 'agentcfg stop'.")
		}
		return err
	case "auth":
		return runAuth(ctx, home, args[1:], input, output, diagnostics)
	case "harness", "harnesses", "skill", "skills", "mcp", "mcps":
		return runCategory(ctx, args[0], flags, home, args[1:], input, output)
	case "config":
		if err := parse(flags, args[1:]); err != nil {
			return helpResult(err)
		}
		return tui.RunMenu(ctx, home, input, output)
	case "init":
		return initialize(ctx, flags, root, args[1:], input, output)
	case "sync":
		return synchronize(ctx, flags, root, args[1:], input, output, diagnostics)
	case "connect":
		name := flags.String("server", "", "canonical MCP server identifier")
		harness := flags.String("harness", "", "calling harness (for filtering)")
		if err := parse(flags, args[1:]); err != nil {
			return helpResult(err)
		}
		if *name == "" {
			return errors.New("connect requires --server")
		}
		return broker.Connect(ctx, *root, *name, *harness, input, output)
	case "daemon", "status", "stop", "list", "validate":
		plain := flags.Bool("plain", false, "plain output without Huh terminal forms")
		var asJSON, yes *bool
		if args[0] == "status" {
			asJSON = flags.Bool("json", false, "always output machine-readable JSON")
		}
		if args[0] == "stop" {
			yes = flags.Bool("yes", false, "skip Huh confirmation in interactive terminals")
		}
		if err := parse(flags, args[1:]); err != nil {
			return helpResult(err)
		}
		return inspectOrServe(ctx, args[0], *root, input, output, diagnostics, terminalPresentation(input, output, *plain), asJSON != nil && *asJSON, yes != nil && *yes)
	default:
		return fmt.Errorf("unknown command %q; run agentcfg help", args[0])
	}
}

// runCategory keeps category commands consistent: each opens its Huh screen
// directly; --plain prints the current effective user-level state and never
// writes files or starts an MCP process.
func runCategory(ctx context.Context, category string, flags *flag.FlagSet, home string, args []string, input io.Reader, output io.Writer) error {
	plain := flags.Bool("plain", false, "show current configuration without opening a Huh form")
	if err := parse(flags, args); err != nil {
		return helpResult(err)
	}
	if !*plain {
		switch category {
		case "harness", "harnesses":
			return tui.Run(ctx, home, input, output)
		case "skill", "skills":
			return tui.RunSkills(ctx, home, input, output)
		default:
			return tui.RunMCP(ctx, home, input, output)
		}
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	switch category {
	case "harness", "harnesses":
		_, err = fmt.Fprintln(output, "Enabled harnesses:", strings.Join(cfg.Settings.Harnesses, ", "))
	case "skill", "skills":
		for _, skill := range cfg.Skills {
			fmt.Fprintf(output, "%s  %s\n", skill.Name, availableHarnesses(cfg.Settings.Harnesses, skill.Harnesses))
		}
	default:
		names := make([]string, 0, len(cfg.Servers))
		for name := range cfg.Servers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			server := cfg.Servers[name]
			fmt.Fprintf(output, "%s  %s  %s\n", name, server.Type, availableHarnesses(cfg.Settings.Harnesses, server.Harnesses))
		}
	}
	return err
}

func availableHarnesses(configured, filter []string) string {
	var enabled []string
	for _, name := range configured {
		if config.Allows(filter, name) {
			enabled = append(enabled, name)
		}
	}
	if len(enabled) == 0 {
		return "disabled for all configured harnesses"
	}
	return strings.Join(enabled, ", ")
}

func parse(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return nil
}

func helpResult(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func inspectOrServe(ctx context.Context, command, root string, input io.Reader, output, diagnostics io.Writer, interactive, asJSON, yes bool) error {
	switch command {
	case "daemon":
		return broker.Serve(ctx, root, diagnostics)
	case "status":
		status, err := broker.GetStatus(ctx, root)
		if err != nil {
			return err
		}
		if interactive && !asJSON {
			var body strings.Builder
			if !status.Running {
				body.WriteString("Broker stopped. No MCP process started by inspection.\n")
			} else {
				fmt.Fprintf(&body, "Broker running (PID %d)\n", status.PID)
			}
			for _, backend := range status.Backends {
				mode := "shared"
				if backend.Isolated {
					mode = "isolated"
				}
				fmt.Fprintf(&body, "%s: %s, PID %d, %d client(s), %d pending\n", backend.Name, mode, backend.PID, backend.Clients, backend.Pending)
			}
			return terminalNote(ctx, input, output, "MCP process status", body.String())
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(status)
	case "stop":
		if interactive && !yes {
			confirmed, err := terminalConfirm(ctx, input, output, "Stop the agentcfg broker?", "Managed MCP processes will be closed. This does not alter user configuration.")
			if err != nil {
				return err
			}
			if !confirmed {
				return nil
			}
		}
		if err := broker.Stop(ctx, root); err != nil {
			return err
		}
		if interactive {
			return terminalNote(ctx, input, output, "Broker stopped", "Shutdown requested for the active user's agentcfg broker.")
		}
		_, err := fmt.Fprintln(output, "Broker shutdown requested.")
		return err
	default:
		cfg, err := config.Load(root)
		if err != nil {
			return err
		}
		for _, warning := range cfg.Warnings {
			fmt.Fprintln(diagnostics, "warning:", warning)
		}
		if command == "list" {
			if interactive {
				var inventory strings.Builder
				if err := printInventory(cfg, &inventory); err != nil {
					return err
				}
				return terminalNote(ctx, input, output, "Discovered MCPs and skills", inventory.String())
			}
			return printInventory(cfg, output)
		}
		message := fmt.Sprintf("Validated %d MCP servers, %d skills, %d harnesses.", len(cfg.Servers), len(cfg.Skills), len(cfg.Settings.Harnesses))
		if interactive {
			if err := terminalNote(ctx, input, output, "Configuration validation", message); err != nil {
				return err
			}
		} else {
			fmt.Fprintln(output, message)
		}
		if len(cfg.Warnings) != 0 {
			return fmt.Errorf("validation has %d warning(s); invalid components are disabled", len(cfg.Warnings))
		}
		return nil
	}
}

func printInventory(cfg *config.Config, output io.Writer) error {
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintln(output, "MCP servers:")
	for _, name := range names {
		server := cfg.Servers[name]
		sharing := server.Sharing
		if sharing == "" {
			sharing = "shared"
		}
		fmt.Fprintf(output, "  %s  [%s, %s]\n", name, server.Type, sharing)
	}
	fmt.Fprintln(output, "Skills:")
	for _, skill := range cfg.Skills {
		fmt.Fprintf(output, "  %s  %s\n", skill.Name, skill.Path)
	}
	_, err := fmt.Fprintln(output, "Harnesses:", strings.Join(cfg.Settings.Harnesses, ", "))
	return err
}

func synchronize(ctx context.Context, flags *flag.FlagSet, root *string, args []string, input io.Reader, output, diagnostics io.Writer) error {
	apply := flags.Bool("apply", false, "apply the proposed changes; default is read-only")
	dryRun := flags.Bool("dry-run", false, "explicit read-only preview")
	plain := flags.Bool("plain", false, "plain output without Huh terminal forms")
	yes := flags.Bool("yes", false, "skip Huh confirmation for --apply")
	scope := "user"
	mode := flags.String("skill-mode", "auto", "auto (symlink, copy fallback), symlink, or copy")
	binary := flags.String("binary", "", "absolute installed agentcfg binary path (defaults to current executable)")
	if err := parse(flags, args); err != nil {
		return helpResult(err)
	}
	if *apply && *dryRun {
		return errors.New("--apply and --dry-run are mutually exclusive")
	}
	cfg, err := config.Load(*root)
	if err != nil {
		return err
	}
	plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: cfg.Root, Binary: *binary, Scope: scope, SkillMode: *mode})
	if err != nil {
		return err
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintln(diagnostics, "warning:", warning)
	}
	var changes strings.Builder
	for _, change := range plan.Changes {
		fmt.Fprintf(&changes, "%s: %s\n", change.Action, change.Path)
	}
	interactive := terminalPresentation(input, output, *plain)
	if interactive {
		title := "User-scope synchronization preview"
		if err := terminalNote(ctx, input, output, title, fmt.Sprintf("%d planned change(s):\n%s\nOriginal SKILL.md files are never rewritten during distribution.", len(plan.Changes), changes.String())); err != nil {
			return err
		}
	} else {
		fmt.Fprint(output, changes.String())
	}
	if !*apply {
		if !interactive {
			fmt.Fprintf(output, "Preview: %d change(s); nothing written. Use --apply to apply.\n", len(plan.Changes))
		}
		return nil
	}
	if interactive && !*yes && len(plan.Changes) > 0 {
		confirmed, err := terminalConfirm(ctx, input, output, "Apply these user-level changes?", "The plan is revalidated before writing. Unmanaged configuration is preserved.")
		if err != nil {
			return err
		}
		if !confirmed {
			return nil
		}
	}
	if err := plan.Apply(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Applied %d change(s).\n", len(plan.Changes))
	return err
}
