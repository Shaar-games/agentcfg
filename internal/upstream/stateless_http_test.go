package upstream

import (
	"net/http"
	"sync/atomic"
	"testing"

	"agentmux/internal/wire"
)

func modernHTTPMessage(id any, method string, params map[string]any) wire.Message {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    wire.ModernProtocolVersion,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "harness", "version": "1"},
	}
	return wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(id), "method": wire.Raw(method), "params": wire.Raw(params)}
}

func TestModernStatelessHTTPNoSessionAndRequiredHeaders(t *testing.T) {
	var discovery, tools, unexpected atomic.Int32
	transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			unexpected.Add(1)
			w.WriteHeader(405)
			return
		}
		message := decodeHTTP(t, r)
		if r.Header.Get("Mcp-Session-Id") != "" || r.Header.Get("MCP-Protocol-Version") != wire.ModernProtocolVersion || r.Header.Get("Mcp-Method") != message.Method() {
			t.Errorf("invalid stateless headers: %s %s %s", r.Header.Get("Mcp-Session-Id"), r.Header.Get("MCP-Protocol-Version"), r.Header.Get("Mcp-Method"))
		}
		switch message.Method() {
		case "server/discover":
			discovery.Add(1)
			if r.Header.Get("Mcp-Name") != "" {
				t.Error("discovery has unexpected name header")
			}
			writeHTTPJSON(w, wire.Result(message.ID(), map[string]any{"supportedVersions": []string{wire.ModernProtocolVersion}, "capabilities": map[string]any{"tools": map[string]any{}}}))
		case "tools/call":
			tools.Add(1)
			if r.Header.Get("Mcp-Name") != "a" && r.Header.Get("Mcp-Name") != "b" {
				t.Error("missing Mcp-Name")
			}
			writeHTTPJSON(w, wire.Result(message.ID(), map[string]any{"ok": true}))
		default:
			t.Errorf("unexpected method: %s", message.Method())
		}
	}))
	if err := transport.Write(modernHTTPMessage(1, "server/discover", nil)); err != nil {
		t.Fatal(err)
	}
	if response := mustRead(t, transport); response["error"] != nil {
		t.Fatal(response)
	}
	if transport.state != httpModern {
		t.Fatalf("not in modern mode: %v", transport.state)
	}
	if err := transport.Write(modernHTTPMessage(2, "tools/call", map[string]any{"name": "a"})); err != nil {
		t.Fatal(err)
	}
	if err := transport.Write(modernHTTPMessage(3, "tools/call", map[string]any{"name": "b"})); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if response := mustRead(t, transport); response["error"] != nil {
			t.Fatal(response)
		}
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	if discovery.Load() != 1 || tools.Load() != 2 || unexpected.Load() != 0 {
		t.Fatalf("request counts: discover=%d tools=%d GET/DELETE=%d", discovery.Load(), tools.Load(), unexpected.Load())
	}
}

func TestModernHTTPDiscoveryErrorCanFallbackToLegacyInitialize(t *testing.T) {
	var discovery, initialized atomic.Int32
	transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		}
		message := decodeHTTP(t, r)
		switch message.Method() {
		case "server/discover":
			discovery.Add(1)
			writeHTTPJSON(w, wire.Error(message.ID(), -32601, "Method not found"))
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "legacy")
			writeHTTPJSON(w, wire.Result(message.ID(), map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}}))
		case "notifications/initialized":
			initialized.Add(1)
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected request %s", message.Method())
		}
	}))
	if err := transport.Write(modernHTTPMessage(1, "server/discover", nil)); err != nil {
		t.Fatal(err)
	}
	if answer := mustRead(t, transport); answer["error"] == nil {
		t.Fatal("failed modern discovery was not delivered")
	}
	if transport.state != httpNew {
		t.Fatalf("failed discovery locked legacy fallback: %v", transport.state)
	}
	initializeHTTP(t, transport)
	if discovery.Load() != 1 || initialized.Load() != 1 {
		t.Fatal("legacy fallback did not complete")
	}
}

func TestModernHTTPDirectRequestWithoutDiscovery(t *testing.T) {
	var requests atomic.Int32
	transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		message := decodeHTTP(t, r)
		if message.Method() != "tools/list" || r.Header.Get("Mcp-Method") != "tools/list" || r.Header.Get("MCP-Protocol-Version") != wire.ModernProtocolVersion {
			t.Errorf("unexpected modern request: %s %v", message.Method(), r.Header)
		}
		requests.Add(1)
		writeHTTPJSON(w, wire.Result(message.ID(), map[string]any{"tools": []any{}}))
	}))
	if err := transport.Write(modernHTTPMessage(7, "tools/list", nil)); err != nil {
		t.Fatal(err)
	}
	if answer := mustRead(t, transport); string(answer.ID()) != "7" || answer["error"] != nil {
		t.Fatal(answer)
	}
	if requests.Load() != 1 {
		t.Fatal("unexpected extra HTTP handshakes")
	}
}
