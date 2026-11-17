package main

import (
	"context"
	"errors"
	"flag"
	"io"

	"agentcfg/internal/broker"
	"agentcfg/internal/config"
)

// The browser belongs to this explicitly opted-in proxy, never the daemon.
// MCP stdin remains available for protocol messages and cancellation throughout
// authorization; all human-readable diagnostics go to stderr.
func runConnect(ctx context.Context, home string, args []string, input io.Reader, output, diagnostics io.Writer, opener func(string) error) error {
	flags := flag.NewFlagSet("connect", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	name := flags.String("server", "", "canonical MCP server identifier")
	harness := flags.String("harness", "", "calling harness (for filtering)")
	autoAuth := flags.Bool("auto-auth", false, "open the browser if OAuth login is required and wait for authorization (overrides oauth.autoOpenBrowser)")
	if err := parse(flags, args); err != nil {
		return helpResult(err)
	}
	if *name == "" {
		return errors.New("connect requires --server")
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	enabled := cfg.Settings.OAuth.AutoOpenBrowser
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "auto-auth" {
			enabled = *autoAuth
		}
	})
	return broker.ConnectWithOptions(ctx, cfg.Root, *name, *harness, input, output, connectAuthorizationOptions(enabled, diagnostics, opener))
}
