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
)

const version = "0.1.0"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agentmux:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, input io.Reader, output, diagnostics io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(output, `agentmux — shared MCP processes and declarative skills

Usage: agentmux <command> [options]

  init       Create empty canonical .agentmux files (never overwrite)
  list       Show discovered MCP servers and skills
  validate   Validate canonical configuration and plugin discovery
  sync       Preview native harness changes; --apply writes them
  connect    Stdio proxy for one MCP; starts the private broker on demand
  status     Show the broker and upstream process IDs (does not start it)
  stop       Stop this root's broker and its managed MCP children
  daemon     Run the local IPC broker in the foreground
  version    Print version

All commands accept --root <home-directory> (default: user home).
Use agentmux <command> --help for command-specific options.
No web server or TCP listener is used. Bidirectional sync is not implemented.`)
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
	root := flags.String("root", home, "home directory containing .agentmux")
	switch args[0] {
	case "init":
		return initialize(flags, root, args[1:], output)
	case "sync":
		return synchronize(flags, root, args[1:], output, diagnostics)
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
		if err := parse(flags, args[1:]); err != nil {
			return helpResult(err)
		}
		return inspectOrServe(ctx, args[0], *root, output, diagnostics)
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

func inspectOrServe(ctx context.Context, command, root string, output, diagnostics io.Writer) error {
	switch command {
	case "daemon":
		return broker.Serve(ctx, root, diagnostics)
	case "status":
		status, err := broker.GetStatus(ctx, root)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(status)
	case "stop":
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
			fmt.Fprintln(diagnostics, "warning:", warning)
		}
		if command == "list" {
			return printInventory(cfg, output)
		}
		fmt.Fprintf(output, "Validated %d MCP servers, %d skills, %d harnesses.\n", len(cfg.Servers), len(cfg.Skills), len(cfg.Settings.Harnesses))
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

func initialize(flags *flag.FlagSet, root *string, args []string, output io.Writer) error {
	harnesses := flags.String("harnesses", "claude-code,codex,cursor,opencode,gemini", "comma-separated native harness identifiers")
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

func synchronize(flags *flag.FlagSet, root *string, args []string, output, diagnostics io.Writer) error {
	apply := flags.Bool("apply", false, "apply the proposed changes; default is read-only")
	dryRun := flags.Bool("dry-run", false, "explicit read-only preview")
	scope := flags.String("scope", "user", "user (default) or project")
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
	plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: cfg.Root, Binary: *binary, Scope: *scope, SkillMode: *mode})
	if err != nil {
		return err
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintln(diagnostics, "warning:", warning)
	}
	for _, change := range plan.Changes {
		fmt.Fprintf(output, "%s: %s\n", change.Action, change.Path)
	}
	if !*apply {
		fmt.Fprintf(output, "Preview: %d change(s); nothing written. Use --apply to apply.\n", len(plan.Changes))
		return nil
	}
	if err := plan.Apply(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Applied %d change(s).\n", len(plan.Changes))
	return err
}
