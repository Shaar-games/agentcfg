package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/broker"
	"agentcfg/internal/wire"
)

// Exercise flag precedence through the real IPC broker and an OAuth/MCP
// fixture. The opener follows only the fixture URL, never a desktop browser.
func TestConnectCLIAutoAuthPolicyAndPendingInitialization(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setting  bool
		flag     string
		wantAuto bool
	}{
		{name: "manual-default"},
		{name: "flag-enables", flag: "--auto-auth", wantAuto: true},
		{name: "setting-enables", setting: true, wantAuto: true},
		{name: "flag-disables-setting", setting: true, flag: "--auto-auth=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENTCFG_OAUTH_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			home := integrationRoot(t, "shared", "30s")
			fixture := newCLIAuthFixture(t)
			definition, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{
				"url": fixture.server.URL + "/mcp", "oauth": map[string]any{
					"clientId": "manual-fixture", "issuer": fixture.server.URL, "redirectUrl": fixture.redirect,
				},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			writeScopeFixture(t, filepath.Join(home, ".agentcfg", "mcp.json"), string(definition))
			writeScopeFixture(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), fmt.Sprintf(
				`{"harnesses":["cursor"],"runtime":{"idleTimeout":"30s","requestTimeout":"10s"},"oauth":{"autoOpenBrowser":%t}}`, tc.setting))
			sources := projectSnapshot(t, filepath.Join(home, ".agentcfg"))
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			brokerDone := make(chan error, 1)
			go func() { brokerDone <- broker.Serve(ctx, home, io.Discard) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-brokerDone:
				case <-time.After(5 * time.Second):
					t.Error("OAuth fixture broker did not stop")
				}
			})
			for {
				state, err := broker.GetStatus(ctx, home)
				if err == nil && state.Running {
					break
				}
				select {
				case err := <-brokerDone:
					// Preserve completion for cleanup.
					brokerDone <- err
					t.Fatalf("fixture broker stopped before readiness: %v", err)
				case <-ctx.Done():
					t.Fatal("fixture broker did not become ready")
				case <-time.After(10 * time.Millisecond):
				}
			}
			in, input := io.Pipe()
			output, out := io.Pipe()
			connectDone := make(chan error, 1)
			opened, consent := make(chan struct{}, 1), make(chan struct{})
			var openCount atomic.Int32
			client := &http.Client{Timeout: 5 * time.Second}
			defer client.CloseIdleConnections()
			opener := func(address string) error {
				openCount.Add(1)
				if !strings.HasPrefix(address, fixture.server.URL+"/authorize?") {
					return fmt.Errorf("unexpected fixture authorization destination")
				}
				select {
				case opened <- struct{}{}:
				default:
				}
				select {
				case <-consent:
				case <-ctx.Done():
					return ctx.Err()
				}
				response, err := client.Get(address)
				if err != nil {
					return err
				}
				defer response.Body.Close()
				if response.StatusCode != http.StatusOK {
					return fmt.Errorf("fixture callback status %d", response.StatusCode)
				}
				return nil
			}
			args := []string{"--server", "fixture", "--harness", "cursor"}
			if tc.flag != "" {
				args = append(args, tc.flag)
			}
			var diagnostics safeBuffer
			go func() { connectDone <- runConnect(ctx, home, args, in, out, &diagnostics, opener) }()
			t.Cleanup(func() {
				cancel()
				_ = input.Close()
				_ = in.Close()
				_ = output.Close()
				_ = out.Close()
				select {
				case <-connectDone:
				case <-time.After(5 * time.Second):
					t.Error("proxy did not stop after cancellation")
				}
			})
			type readResult struct {
				message wire.Message
				err     error
			}
			reply := make(chan readResult, 1)
			go func() { message, err := wire.NewReader(output).Read(); reply <- readResult{message, err} }()
			if err := wire.NewWriter(input).Write(wire.Message{
				"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(1), "method": wire.Raw("initialize"),
				"params": json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"cli-fixture","version":"1"}}`),
			}); err != nil {
				t.Fatal(err)
			}
			if tc.wantAuto {
				select {
				case <-opened:
				case result := <-reply:
					t.Fatalf("initialization replied before browser consent: %s %v", result.message, result.err)
				case <-ctx.Done():
					t.Fatal("automatic authorization did not start")
				}
				select {
				case result := <-reply:
					t.Fatalf("initialization was not held pending: %s %v", result.message, result.err)
				case <-time.After(100 * time.Millisecond):
				}
				close(consent)
			}
			select {
			case result := <-reply:
				if result.err != nil {
					t.Fatalf("invalid MCP stdout: %v", result.err)
				}
				if tc.wantAuto {
					if result.message["error"] != nil || !strings.Contains(string(result.message["result"]), "manual-login-fixture") {
						t.Fatalf("upstream initialization did not resume: %s", result.message)
					}
					if openCount.Load() != 1 || fixture.exchanges.Load() != 1 {
						t.Fatalf("unexpected auth attempts: browser=%d tokens=%d", openCount.Load(), fixture.exchanges.Load())
					}
				} else {
					if !strings.Contains(string(result.message["error"]), "agentcfg auth login fixture") || result.message["result"] != nil {
						t.Fatalf("manual mode did not return the login action: %s", result.message)
					}
					if openCount.Load() != 0 || fixture.exchanges.Load() != 0 {
						t.Fatal("manual mode attempted interactive authorization")
					}
				}
			case <-ctx.Done():
				t.Fatalf("initialization did not finish: %s", diagnostics.String())
			}
			for _, secret := range []string{"fixture-access", "fixture-refresh", "fixture-code", "code_challenge=", "state="} {
				if strings.Contains(diagnostics.String(), secret) {
					t.Fatalf("automatic diagnostics disclosed OAuth material %q", secret)
				}
			}
			// Login may add encrypted state, but canonical source bytes stay exact.
			after := projectSnapshot(t, filepath.Join(home, ".agentcfg"))
			for _, name := range []string{"mcp.json", "agentcfg.json"} {
				if sources[name] != after[name] {
					t.Fatalf("connection modified canonical %s", name)
				}
			}
		})
	}
}
