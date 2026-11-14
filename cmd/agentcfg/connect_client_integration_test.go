package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/wire"
)

// Exercise the installed CLI wiring, its actual private IPC broker, the MCP
// elicitation exchange and the production loopback callback together. PATH is
// empty of programs, so even a regression cannot launch a real browser.
func TestCLIClientOAuthUsesURLPromptAndReusesStoredGrant(t *testing.T) {
	binary := cliBinary(t)
	t.Setenv("AGENTCFG_OAUTH_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	fixture := newCLIAuthFixture(t)
	root := integrationRoot(t, "shared", "10s")
	definition, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{
		"url": fixture.server.URL + "/mcp", "oauth": map[string]any{
			"clientId": "manual-fixture", "issuer": fixture.server.URL, "redirectUrl": fixture.redirect,
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately omit oauth.autoOpenBrowser to verify its default.
	settings := []byte(`{"harnesses":["cursor"],"runtime":{"idleTimeout":"10s","requestTimeout":"15s"}}`)
	for name, data := range map[string][]byte{"mcp.json": definition, "agentcfg.json": settings} {
		if err := os.WriteFile(filepath.Join(root, ".agentcfg", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", t.TempDir())
	peer := startCLIPeer(t, binary, root, "cursor")
	var transcript bytes.Buffer
	read := func(p *cliPeer) wire.Message {
		message := p.read(t)
		if string(message["jsonrpc"]) != `"2.0"` {
			t.Fatal("CLI stdout contained a non-MCP control message")
		}
		transcript.Write(wire.Raw(message))
		return message
	}
	send := func(p *cliPeer, id, method string, params any) {
		t.Helper()
		if err := p.writer.Write(wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(id), "method": wire.Raw(method), "params": wire.Raw(params)}); err != nil {
			t.Fatal(err)
		}
	}
	initialize := func(p *cliPeer, id string, urlSupport bool) {
		capabilities := map[string]any{}
		if urlSupport {
			capabilities["elicitation"] = map[string]any{"url": map[string]any{}}
		}
		send(p, id, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": capabilities,
			"clientInfo": map[string]string{"name": "cli-url-fixture", "version": "1"}})
		response := read(p)
		if !bytes.Equal(response.ID(), wire.Raw(id)) || response.Method() != "" {
			t.Fatal("CLI requested user input before MCP initialization completed")
		}
		if err := p.writer.Write(wire.Notification("notifications/initialized", nil)); err != nil {
			t.Fatal(err)
		}
	}
	assertCatalog := func(p *cliPeer, id, wantName string) {
		send(p, id, "tools/list", map[string]any{})
		message := read(p)
		var result struct {
			Tools []struct{ Name string } `json:"tools"`
		}
		if !bytes.Equal(message.ID(), wire.Raw(id)) || json.Unmarshal(message["result"], &result) != nil || len(result.Tools) != 1 || result.Tools[0].Name != wantName {
			t.Fatalf("CLI did not expose the expected %s tool", wantName)
		}
	}
	initialize(peer, "initialize", true)
	assertCatalog(peer, "locked-tools", "agentcfg_authenticate")
	send(peer, "authenticate", "tools/call", map[string]any{"name": "agentcfg_authenticate", "arguments": map[string]any{}})
	prompt := read(peer)
	var presentation struct {
		Mode          string `json:"mode"`
		URL           string `json:"url"`
		ElicitationID string `json:"elicitationId"`
	}
	if prompt.Method() != "elicitation/create" || !prompt.HasID() || json.Unmarshal(prompt["params"], &presentation) != nil ||
		presentation.Mode != "url" || presentation.ElicitationID == "" || !strings.HasPrefix(presentation.URL, fixture.server.URL+"/authorize?") {
		t.Fatal("CLI did not request URL elicitation for the local OAuth provider")
	}
	if err := peer.writer.Write(wire.Result(prompt.ID(), map[string]string{"action": "accept"})); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	callback, err := client.Get(presentation.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, callback.Body)
	_ = callback.Body.Close()
	if callback.StatusCode != http.StatusOK {
		t.Fatalf("production OAuth callback returned HTTP %d", callback.StatusCode)
	}
	for {
		message := read(peer)
		if message.Method() != "" {
			if message.HasID() {
				t.Fatal("CLI requested a second authorization")
			}
			switch message.Method() {
			case "notifications/elicitation/complete", "notifications/tools/list_changed", "notifications/resources/list_changed", "notifications/prompts/list_changed":
				continue
			default:
				t.Fatalf("unexpected CLI notification %s", message.Method())
			}
		}
		var result struct{ IsError bool }
		if !bytes.Equal(message.ID(), wire.Raw("authenticate")) || json.Unmarshal(message["result"], &result) != nil || result.IsError {
			t.Fatal("CLI did not complete the OAuth tool call")
		}
		break
	}
	assertCatalog(peer, "unlocked-tools", "fixture-echo")
	peer.close(t)
	// The next process has no elicitation capability and must still reuse the
	// grant stored by the first process through the shared broker.
	second := startCLIPeer(t, binary, root, "cursor")
	initialize(second, "reconnect", false)
	assertCatalog(second, "reconnected-tools", "fixture-echo")
	second.close(t)
	if fixture.exchanges.Load() != 1 {
		t.Fatalf("reconnect performed %d token exchanges; want one", fixture.exchanges.Load())
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	state, err := auth.Inspect(ctx, cfg.Servers["fixture"], auth.Options{Home: root})
	if err != nil || !state.Authenticated || !state.Stored {
		t.Fatalf("CLI did not persist usable OAuth credentials: %+v, %v", state, err)
	}
	for name, before := range map[string][]byte{"mcp.json": definition, "agentcfg.json": settings} {
		after, err := os.ReadFile(filepath.Join(root, ".agentcfg", name))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("CLI OAuth changed %s: %v", name, err)
		}
	}
	diagnostics := peer.stderr.String() + second.stderr.String()
	if !strings.Contains(diagnostics, "Complete OAuth sign-in in your MCP client.") || strings.Contains(diagnostics, "Opening your browser") {
		t.Fatal("CLI did not keep client elicitation separate from automatic browser opening")
	}
	for _, secret := range []string{"fixture-code", "fixture-access", "fixture-refresh"} {
		if strings.Contains(transcript.String()+diagnostics, secret) {
			t.Fatal("CLI exposed fixture credentials on MCP stdout or stderr")
		}
	}
	assertConnectOAuthCallbackClosed(t, fixture.callbackAddress)
}
