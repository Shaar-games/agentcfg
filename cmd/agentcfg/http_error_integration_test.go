package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/broker"
	"agentcfg/internal/ipc"
	"agentcfg/internal/wire"
)

func TestCLIHTTPInitializeErrorKeepsSafeDiagnostic(t *testing.T) {
	binary := cliBinary(t)
	const (
		pathSecret           = "fixture-private-endpoint"
		querySecret          = "fixture-query-credential"
		requestHeaderSecret  = "fixture-request-credential"
		responseHeaderSecret = "fixture-response-header-secret"
		responseBodySecret   = "fixture-response-body-secret"
	)
	var requests atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/"+pathSecret {
			t.Error("unexpected HTTP request: initialization must not be replayed or trigger OAuth discovery")
		}
		if r.URL.Query().Get("api_key") != querySecret || r.Header.Get("Authorization") != "Bearer "+requestHeaderSecret {
			t.Error("HTTP fixture did not receive its configured credentials")
		}
		var request wire.Message
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Method() != "initialize" || !request.HasID() {
			t.Error("HTTP fixture did not receive an initialize request")
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+responseHeaderSecret+`", resource_metadata="http://`+r.Host+`/oauth-metadata"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(responseBodySecret))
	}))
	t.Cleanup(fixture.Close)
	root := integrationRoot(t, "shared", "10s")
	// OAuth is deliberately absent: an HTTP challenge must not start an
	// interactive authorization flow or inspect credentials outside this home.
	config := map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{
		"type": "streamable-http", "url": fixture.URL + "/" + pathSecret + "?api_key=" + querySecret,
		"headers": map[string]string{"Authorization": "Bearer " + requestHeaderSecret},
	}}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".agentcfg", "mcp.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	peer := startCLIPeer(t, binary, root, "cursor")
	requestID := wire.Raw("client-initialize-401")
	if err := peer.writer.Write(wire.Message{
		"jsonrpc": wire.Raw("2.0"), "id": requestID, "method": wire.Raw("initialize"),
		"params": wire.Raw(map[string]any{
			"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
			"clientInfo": map[string]string{"name": "http-error-fixture", "version": "1"},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	// cliPeer.read intentionally rejects MCP errors; this test needs the actual
	// error frame returned through the CLI's stdout and broker IPC connection.
	type outcome struct {
		message wire.Message
		err     error
	}
	reply := make(chan outcome, 1)
	go func() {
		message, err := peer.reader.Read()
		reply <- outcome{message, err}
	}()
	var message wire.Message
	select {
	case result := <-reply:
		if result.err != nil {
			t.Fatalf("read HTTP failure from CLI stdout: %v; stderr: %s", result.err, peer.stderr.String())
		}
		message = result.message
	case <-time.After(20 * time.Second):
		t.Fatal("CLI did not return the HTTP initialization error")
	}
	if !bytes.Equal(message.ID(), requestID) || message["result"] != nil {
		t.Fatalf("HTTP failure was not correlated to the client request: %s", wire.Raw(message))
	}
	var failure struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(message["error"], &failure); err != nil || failure.Code != -32000 {
		t.Fatalf("expected an MCP transport error, got %s", wire.Raw(message))
	}
	for _, want := range []string{"HTTP 401", "authentication", "the operation is not retried"} {
		if !strings.Contains(failure.Message, want) {
			t.Errorf("HTTP diagnostic missing %q: %q", want, failure.Message)
		}
	}
	peer.close(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := broker.Stop(ctx, root); err != nil {
		t.Fatal(err)
	}
	fixture.Close()
	if got := requests.Load(); got != 1 {
		t.Errorf("HTTP initialization made %d requests; want one POST without replay or discovery", got)
	}
	endpoint, err := ipc.ForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	brokerLog, err := os.ReadFile(filepath.Join(endpoint.Directory, "broker.log"))
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := string(wire.Raw(message)) + peer.stderr.String() + string(brokerLog)
	for _, secret := range []string{fixture.URL, pathSecret, querySecret, requestHeaderSecret, responseHeaderSecret, responseBodySecret} {
		if strings.Contains(diagnostics, secret) {
			t.Errorf("HTTP error exposed fixture secret %q on stdio or in the broker log", secret)
		}
	}
}
