package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/gateway"
)

func runGateway(ctx context.Context, root string, args []string, output, diagnostics io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(output, `Usage: agentcfg gateway <start|run|stop|status|url> [options]

start        Start the native OAuth HTTP gateway in the background
run          Run it in the foreground (for a user service or login startup)
stop         Stop only the gateway; preserve the stdio broker
status       Show gateway status without starting it
url          Print one configured HTTP MCP URL (--server NAME [--harness NAME])

Enable gateway.enabled in ~/.agentcfg/agentcfg.json before starting.
gateway.listen defaults to 127.0.0.1:43877; only loopback IP addresses are allowed.
gateway.tlsCert and gateway.tlsKey optionally enable HTTPS with your certificate.
gateway.harnesses optionally selects native HTTP clients; others retain stdio.
Start the gateway once before syncing its protected HTTP URLs to selected clients.
A client URL alone does not start a process: run gateway start, or configure
gateway run at login.
OAuth uses the encrypted agentcfg credential store. No MCP configuration is
modified by gateway start/run/stop/status/url.`)
		return err
	}
	command := args[0]
	flags := flag.NewFlagSet("gateway "+command, flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	var name, harness *string
	if command == "url" {
		name = flags.String("server", "", "canonical HTTP MCP server identifier")
		harness = flags.String("harness", "", "calling harness, if any")
	}
	if command != "start" && command != "run" && command != "stop" && command != "status" && command != "url" {
		return fmt.Errorf("unknown gateway command %q; run agentcfg gateway --help", command)
	}
	if err := parse(flags, args[1:]); err != nil {
		return helpResult(err)
	}
	switch command {
	case "run":
		return gateway.Run(ctx, root, diagnostics)
	case "start":
		status, err := gateway.Start(ctx, root)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "Native OAuth gateway running at %s (PID %d).\n", status.URL, status.PID)
		return err
	case "stop":
		if err := gateway.Stop(ctx, root); err != nil {
			return err
		}
		_, err := fmt.Fprintln(output, "Native OAuth gateway stopped.")
		return err
	case "status":
		status, err := gateway.GetStatus(ctx, root)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(status)
	case "url":
		if *name == "" {
			return errors.New("gateway url requires --server")
		}
		cfg, err := config.Load(root)
		if err != nil {
			return err
		}
		if !cfg.Settings.Gateway.Enabled {
			return errors.New("native OAuth gateway is disabled; set gateway.enabled to true in agentcfg.json")
		}
		entry, found := cfg.Servers[*name]
		if !found || (entry.Type != "http" && entry.Type != "streamable-http") || !config.Allows(entry.Harnesses, *harness) || !config.GatewayForHarness(cfg.Settings.Gateway, *harness) {
			return errors.New("server is not an available HTTP MCP for this harness")
		}
		if *harness != "" {
			found := false
			for _, selected := range cfg.Settings.Harnesses {
				found = found || selected == *harness
			}
			if !found {
				return errors.New("harness is not enabled")
			}
		}
		cfg.Settings.Gateway.RouteKey, err = auth.GatewayKey(ctx, cfg.Root, nil, false)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, config.GatewayResourceURL(cfg.Settings.Gateway, *name, *harness))
		return err
	}
	return nil
}
