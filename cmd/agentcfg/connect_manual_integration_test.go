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

	"agentcfg/internal/wire"
)

func TestCLIStdioManualLoginKeepsRealTools(t *testing.T) {
	binary := cliBinary(t)
	t.Setenv("AGENTCFG_OAUTH_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	fixture := newCLIAuthFixture(t)
	root := integrationRoot(t, "shared", "10s")
	definition, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{"url": fixture.server.URL + "/mcp", "oauth": map[string]any{"clientId": "manual-fixture", "issuer": fixture.server.URL, "redirectUrl": fixture.redirect}}}})
	if err != nil {
		t.Fatal(err)
	}
	settings := []byte(`{"harnesses":["cursor"],"runtime":{"idleTimeout":"10s","requestTimeout":"15s"}}`)
	for name, data := range map[string][]byte{"mcp.json": definition, "agentcfg.json": settings} {
		if err := os.WriteFile(filepath.Join(root, ".agentcfg", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Even a policy regression cannot launch a real browser from this fixture.
	t.Setenv("PATH", t.TempDir())
	peer := startCLIPeer(t, binary, root, "cursor")
	readRaw := func() wire.Message {
		t.Helper()
		type outcome struct {
			message wire.Message
			err     error
		}
		finished := make(chan outcome, 1)
		go func() { message, err := peer.reader.Read(); finished <- outcome{message, err} }()
		select {
		case got := <-finished:
			if got.err != nil {
				t.Fatal(got.err)
			}
			return got.message
		case <-time.After(10 * time.Second):
			t.Fatal("stdio response timed out")
			return nil
		}
	}
	initialize := func(id string) {
		t.Helper()
		if err := peer.writer.Write(wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(id), "method": wire.Raw("initialize"), "params": wire.Raw(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"elicitation": map[string]any{"url": map[string]any{}}}, "clientInfo": map[string]string{"name": "manual-client", "version": "1"}})}); err != nil {
			t.Fatal(err)
		}
	}
	initialize("before-login")
	failure := readRaw()
	if failure.Method() != "" || failure["error"] == nil || !strings.Contains(string(failure["error"]), "agentcfg auth login") {
		t.Fatal("CLI did not keep the ordinary MCP error and manual-login instruction")
	}
	if fixture.exchanges.Load() != 0 || strings.Contains(peer.stderr.String(), "Opening your browser") {
		t.Fatal("default CLI started browser authentication")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	output := &manualOAuthURLConsumer{client: client, authorizePrefix: fixture.server.URL + "/authorize?"}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := runAuth(ctx, root, []string{"login", "fixture", "--no-browser"}, strings.NewReader(""), output, io.Discard); err != nil {
		t.Fatal(err)
	}
	initialize("after-login")
	response := readRaw()
	if response["error"] != nil || response.Method() != "" || !strings.Contains(string(response["result"]), "manual-login-fixture") {
		t.Fatal("CLI did not reuse credentials or changed upstream initialization")
	}
	if err := peer.writer.Write(wire.Notification("notifications/initialized", nil)); err != nil {
		t.Fatal(err)
	}
	if err := peer.writer.Write(wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw("tools"), "method": wire.Raw("tools/list"), "params": wire.Raw(map[string]any{})}); err != nil {
		t.Fatal(err)
	}
	catalog := readRaw()
	if catalog["error"] != nil || !strings.Contains(string(catalog["result"]), "fixture-echo") || strings.Contains(string(catalog["result"]), "agentcfg_authenticate") {
		t.Fatal("CLI did not expose only the actual upstream catalog")
	}
	if fixture.exchanges.Load() != 1 || strings.Contains(peer.stderr.String(), "fixture-access") {
		t.Fatal("CLI repeated login or exposed a credential")
	}
	for name, before := range map[string][]byte{"mcp.json": definition, "agentcfg.json": settings} {
		after, err := os.ReadFile(filepath.Join(root, ".agentcfg", name))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("manual stdio flow modified %s", name)
		}
	}
	assertConnectOAuthCallbackClosed(t, fixture.callbackAddress)
}
