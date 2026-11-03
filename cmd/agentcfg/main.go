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

	"agentcfg/internal/broker"
	"agentcfg/internal/config"
	"agentcfg/internal/migrate"
	"agentcfg/internal/pathutil"
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
  harnesses             Open the Huh harness checkbox list directly

  init       Create empty canonical .agentcfg files (never overwrite)
  migrate    Preview legacy ~/.agentmux migration; --apply copies securely
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
Existing ~/.agentmux data is never migrated or overwritten automatically.
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
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if args[0] != "migrate" && args[0] != "update" {
		if err := requireMigration(home); err != nil {
			return err
		}
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
	case "harnesses":
		if err := parse(flags, args[1:]); err != nil {
			return helpResult(err)
		}
		return tui.Run(ctx, home, input, output)
	case "config", "menu", "ui", "tui":
		if err := parse(flags, args[1:]); err != nil {
			return helpResult(err)
		}
		return tui.RunMenu(ctx, home, input, output)
	case "migrate":
		return migrateLegacy(flags, home, args[1:], output)
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

// Do not silently start a second configuration while native adapters may
// still point at an unmigrated legacy installation.
func requireMigration(home string) error {
	_, legacyErr := os.Lstat(filepath.Join(home, ".agentmux"))
	_, currentErr := os.Lstat(filepath.Join(home, ".agentcfg"))
	if legacyErr == nil && errors.Is(currentErr, os.ErrNotExist) {
		return errors.New("legacy ~/.agentmux exists but ~/.agentcfg does not; preview 'agentcfg migrate' then use 'agentcfg migrate --apply' before configuring or syncing (nothing changed)")
	}
	if legacyErr != nil && !errors.Is(legacyErr, os.ErrNotExist) {
		return legacyErr
	}
	if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
		return currentErr
	}
	return nil
}

func migrateLegacy(flags *flag.FlagSet, home string, args []string, output io.Writer) error {
	apply := flags.Bool("apply", false, "copy legacy sources securely to ~/.agentcfg (without removing ~/.agentmux)")
	if err := parse(flags, args); err != nil {
		return helpResult(err)
	}
	var (
		report migrate.Report
		err    error
	)
	if *apply {
		report, err = migrate.Apply(home)
	} else {
		report, err = migrate.Preview(home)
	}
	if err != nil {
		return err
	}
	action := "Preview only; nothing written."
	if *apply {
		action = "New canonical configuration copied and verified. Original kept intact."
	}
	fmt.Fprintf(output, "%s\nFrom: %s\nTo:   %s\n%d files, %d directories, %d bytes\n",
		action, report.Legacy, report.Destination, report.Files, report.Directories, report.Bytes)
	for index, path := range report.Paths {
		if index >= 100 {
			fmt.Fprintf(output, "  ... and %d additional paths (all validated before applying)\n", len(report.Paths)-index)
			break
		}
		fmt.Fprintf(output, "  copy %s\n", path)
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(output, "Note: %s\n", warning)
	}
	return nil
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
	var names []string
	if strings.TrimSpace(*harnesses) != "" {
		names = strings.Split(*harnesses, ",")
		for i := range names {
			names[i] = strings.TrimSpace(names[i])
		}
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
	names, err = config.ValidateUserHarnesses(names)
	if err != nil {
		return fmt.Errorf("init harness selection: %w", err)
	}
	agentDir := filepath.Join(abs, ".agentcfg")
	if err := ensureRealCanonicalDirectory(agentDir); err != nil {
		return fmt.Errorf("init source directory: %w", err)
	}
	// Preflight every canonical child directory before writing either source
	// document, so an existing linked skills/plugins directory cannot leave a
	// partly initialized configuration or redirect future writes outside home.
	for _, name := range []string{"skills", "plugins"} {
		if err := ensureRealCanonicalDirectory(filepath.Join(agentDir, name)); err != nil {
			return fmt.Errorf("init %s directory: %w", name, err)
		}
	}
	settings := config.Settings{Harnesses: names, Runtime: config.Runtime{IdleTimeout: "5m", RequestTimeout: "5m"}}
	files := []struct {
		name  string
		value any
	}{
		{"mcp.json", map[string]any{"mcpServers": map[string]any{}}},
		{"agentcfg.json", settings},
	}
	for _, source := range files {
		path := filepath.Join(agentDir, source.name)
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
		encodeErr := encoder.Encode(source.value)
		closeErr := file.Close()
		if encodeErr != nil {
			return encodeErr
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Fprintln(output, "Created", path)
	}
	return nil
}

// A symlink/junction at ~/.agentcfg must not redirect initialization into
// some other directory (including an open project). Only real directories are
// acceptable. The caller validates its harness selection before creating any
// source files.
func ensureRealCanonicalDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || pathutil.IsRedirected(info) {
		return fmt.Errorf("%s is not a real directory (symlinks/junctions are refused)", path)
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
