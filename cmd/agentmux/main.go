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
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"agentmux/internal/broker"
	"agentmux/internal/config"
	"agentmux/internal/syncer"
	"agentmux/internal/tui"
)

// Set at release build time: -ldflags '-X main.version=v0.2.1'.
var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agentmux:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, input io.Reader, output, diagnostics io.Writer) error {
	if len(args) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		return tui.Run(ctx, home, input, output)
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(output, `agentmux — shared MCP processes and declarative skills

Usage: agentmux [command] [options]

  (no command) / ui     Open Charmbracelet Huh configuration menus

  init       Create empty canonical .agentmux files (never overwrite)
  list       Show discovered MCP servers and skills
  validate   Validate canonical configuration and plugin discovery
  sync       Preview native harness changes; --apply writes them
  connect    Stdio proxy for one MCP; starts the private broker on demand
  auth       Explicit OAuth login, status, or logout for a configured MCP
  status     Show the broker and upstream process IDs (does not start it)
  stop       Stop this root's broker and its managed MCP children
  daemon     Run the local IPC broker in the foreground
  version    Print version

All commands use the current user's ~/.agentmux configuration.
	Human-facing CLI commands use Huh forms in a TTY; --plain keeps script output.
Use agentmux <command> --help for command-specific options.
No persistent web server or TCP listener is used. OAuth login may temporarily
listen on a loopback callback address when explicitly invoked.
Bidirectional sync is not implemented.`)
		return err
	}
	if args[0] == "version" {
		_, err := fmt.Fprintln(output, version)
		return err
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	root := &home
	switch args[0] {
	case "auth":
		return runAuth(ctx, home, args[1:], input, output, diagnostics)
	case "ui", "tui":
		if err := parse(flags, args[1:]); err != nil {
			return helpResult(err)
		}
		return tui.Run(ctx, home, input, output)
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
		return fmt.Errorf("unknown command %q; run agentmux help", args[0])
	}
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
			confirmed, err := terminalConfirm(ctx, input, output, "Stop the AgentMux broker?", "Managed MCP processes will be closed. This does not alter user configuration.")
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
			return terminalNote(ctx, input, output, "Broker stopped", "Shutdown requested for the active user's AgentMux broker.")
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

func initialize(ctx context.Context, flags *flag.FlagSet, root *string, args []string, input io.Reader, output io.Writer) error {
	harnesses := flags.String("harnesses", "claude-code,codex,cursor,opencode,gemini", "comma-separated native harness identifiers")
	plain := flags.Bool("plain", false, "skip Huh interactive harness selection")
	if err := parse(flags, args); err != nil {
		return helpResult(err)
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	names := strings.Split(*harnesses, ",")
	for i := range names {
		names[i] = strings.TrimSpace(names[i])
	}
	explicit := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "harnesses" {
			explicit = true
		}
	})
	if !explicit && terminalPresentation(input, output, *plain) {
		names, err = terminalHarnessSelection(ctx, input, output, names)
		if err != nil {
			return err
		}
	}
	settings := config.Settings{Harnesses: names, Runtime: config.Runtime{IdleTimeout: "5m", RequestTimeout: "5m"}}
	files := map[string]any{"mcp.json": map[string]any{"mcpServers": map[string]any{}}, "agentmux.json": settings}
	for name, value := range files {
		path := filepath.Join(abs, ".agentmux", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			fmt.Fprintln(output, "Preserved", path)
			continue
		}
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		encodeErr := encoder.Encode(value)
		closeErr := file.Close()
		if encodeErr != nil {
			return encodeErr
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Fprintln(output, "Created", path)
	}
	for _, name := range []string{"skills", "plugins"} {
		if err := os.MkdirAll(filepath.Join(abs, ".agentmux", name), 0o700); err != nil {
			return err
		}
	}
	return nil
}

func synchronize(ctx context.Context, flags *flag.FlagSet, root *string, args []string, input io.Reader, output, diagnostics io.Writer) error {
	apply := flags.Bool("apply", false, "apply the proposed changes; default is read-only")
	dryRun := flags.Bool("dry-run", false, "explicit read-only preview")
	plain := flags.Bool("plain", false, "plain output without Huh terminal forms")
	yes := flags.Bool("yes", false, "skip Huh confirmation for --apply")
	scope := "user"
	mode := flags.String("skill-mode", "auto", "auto (symlink, copy fallback), symlink, or copy")
	binary := flags.String("binary", "", "absolute installed agentmux binary path (defaults to current executable)")
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
