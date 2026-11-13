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

	"github.com/charmbracelet/huh"
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(output, `agentcfg — shared MCP processes and declarative skills

Usage: agentcfg [command] [options]

Configuration:
  config                Open the configuration menu
  harness, harnesses    Configure enabled AI harnesses
  skill, skills         Configure skill availability
  mcp, mcps             Configure MCP availability and sharing

Setup and inspection:
  init                  Discover existing harnesses, then confirm setup and sync
  restore               Restore pre-init native settings and archive agentcfg sources
  list                  Browse configured MCP servers, skills and harnesses
  validate              Validate the configuration
  sync                  Preview changes; --apply synchronizes native settings

Runtime:
  connect               Stdio proxy for one MCP
  auth                  OAuth login, status or logout
  status                Show broker and process status
  stop                  Stop broker and managed MCP processes
  daemon                Run local IPC broker

Other:
  completion <shell>    Generate Bash, Zsh, Fish or PowerShell completion
  update                Update the executable
  version               Print the installed version
  help                  Show this help (also displayed without arguments)

Use agentcfg <command> --help for options.`)
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
		return configure(ctx, home, input, output)
	case "restore":
		return restore(ctx, flags, home, args[1:], input, output)
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
		return broker.ConnectWithOptions(ctx, *root, *name, *harness, input, output, broker.ConnectOptions{
			AuthorizationCodeFetcher: connectAuthorizationCodeFetcher(diagnostics, openBrowser),
		})
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
	var body strings.Builder
	switch category {
	case "harness", "harnesses":
		fmt.Fprintln(&body, "Enabled harnesses:", strings.Join(cfg.Settings.Harnesses, ", "))
	case "skill", "skills":
		for _, skill := range cfg.Skills {
			fmt.Fprintf(&body, "%s  %s\n", skill.Name, availableHarnesses(cfg.Settings.Harnesses, skill.Harnesses))
		}
	default:
		names := make([]string, 0, len(cfg.Servers))
		for name := range cfg.Servers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			server := cfg.Servers[name]
			fmt.Fprintf(&body, "%s  %s  %s\n", name, server.Type, availableHarnesses(cfg.Settings.Harnesses, server.Harnesses))
		}
	}
	_, err = io.WriteString(output, body.String())
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
			confirmed, err := terminalConfirm(ctx, input, output, "Managed MCP processes will be closed. This does not alter user configuration.", "")
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

		_, err := fmt.Fprintln(output, "Broker shutdown requested.")
		return err
	default:
		cfg, err := config.Load(root)
		if err != nil {
			return err
		}
		for _, warning := range cfg.Warnings {
			if _, err := fmt.Fprintln(diagnostics, "warning:", warning); err != nil {
				return err
			}
		}
		if command == "list" {
			if interactive {
				return tui.RunList(ctx, root, input, output)
			}
			return printInventory(cfg, output)
		}
		message := fmt.Sprintf("Validated %d MCP servers, %d skills, %d harnesses.", len(cfg.Servers), len(cfg.Skills), len(cfg.Settings.Harnesses))
		if interactive {
			if err := terminalNote(ctx, input, output, "Configuration validation", message); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintln(output, message); err != nil {
				return err
			}
		}
		if len(cfg.Warnings) != 0 {
			return fmt.Errorf("validation has %d warning(s); invalid components are disabled", len(cfg.Warnings))
		}
		return nil
	}
}

func printInventory(cfg *config.Config, output io.Writer) error {
	var body strings.Builder
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintln(&body, "MCP servers:")
	for _, name := range names {
		server := cfg.Servers[name]
		sharing := server.Sharing
		if sharing == "" {
			sharing = "shared"
		}
		fmt.Fprintf(&body, "  %s  [%s, %s]\n", name, server.Type, sharing)
	}
	fmt.Fprintln(&body, "Skills:")
	for _, skill := range cfg.Skills {
		fmt.Fprintf(&body, "  %s  %s\n", skill.Name, skill.Path)
	}
	fmt.Fprintln(&body, "Harnesses:", strings.Join(cfg.Settings.Harnesses, ", "))
	_, err := io.WriteString(output, body.String())
	return err
}

func synchronize(ctx context.Context, flags *flag.FlagSet, root *string, args []string, input io.Reader, output, diagnostics io.Writer) error {
	apply := flags.Bool("apply", false, "apply the proposed changes; default is read-only")
	dryRun := flags.Bool("dry-run", false, "explicit read-only preview")
	plain := flags.Bool("plain", false, "plain output without Huh terminal forms")
	yes := flags.Bool("yes", false, "skip Huh confirmation for --apply")
	scope := "user"
	mode := flags.String("skill-mode", "copy", "copy (default), auto (symlink, copy fallback), or symlink")
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
		if _, err := fmt.Fprintln(diagnostics, "warning:", warning); err != nil {
			return err
		}
	}
	var changes strings.Builder
	for _, change := range plan.Changes {
		fmt.Fprintf(&changes, "%s: %s\n", change.Action, change.Path)
	}
	interactive := terminalPresentation(input, output, *plain)
	if interactive {
		title := "Sync preview"
		if err := terminalNote(ctx, input, output, title, fmt.Sprintf("%d file(s)\n%s", len(plan.Changes), changes.String())); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
	} else {
		if _, err := io.WriteString(output, changes.String()); err != nil {
			return err
		}
	}
	if !*apply {
		if !interactive {
			_, err := fmt.Fprintf(output, "Preview: %d change(s); nothing written. Use --apply to apply.\n", len(plan.Changes))
			return err
		}
		return nil
	}
	if interactive && !*yes && len(plan.Changes) > 0 {
		confirmed, err := terminalConfirm(ctx, input, output, "Apply changes?", "")
		if err != nil {
			return err
		}
		if !confirmed {
			return nil
		}
	}
	// Cancellation before the transaction starts must not authorize a write.
	// Once Apply starts, it retains its own all-or-rollback guarantees.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := plan.Apply(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Applied %d change(s).\n", len(plan.Changes))
	return err
}
