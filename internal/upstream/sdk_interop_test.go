package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"agentcfg/internal/wire"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestOfficialSDKServerInterop(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, jsonResponse := range []bool{false, true} {
			name := "legacy"
			if modern {
				name = "modern"
			}
			if jsonResponse {
				name += "/json"
			} else {
				name += "/sse"
			}
			t.Run(name, func(t *testing.T) {
				server := mcp.NewServer(&mcp.Implementation{Name: "sdk-fixture", Version: "1"}, nil)
				server.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{
					"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}},
				}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					var arguments struct{ Value string }
					if err := json.Unmarshal(req.Params.Arguments, &arguments); err != nil {
						return nil, err
					}
					if modern && req.ProtocolVersion() != wire.ModernProtocolVersion {
						t.Errorf("SDK lost modern protocol: %q", req.ProtocolVersion())
					}
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: arguments.Value}}}, nil
				})
				handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
					&mcp.StreamableHTTPOptions{Stateless: modern, JSONResponse: jsonResponse, PropagateRequestCancellation: true})
				transport := newHTTPFixture(t, handler)
				if modern {
					if err := transport.Write(modernHTTPMessage("discover-sdk", "server/discover", nil)); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := transport.Write(rpcRequest("init-sdk", "initialize", map[string]any{
						"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
						"clientInfo": map[string]any{"name": "agentcfg-test", "version": "1"},
					})); err != nil {
						t.Fatal(err)
					}
				}
				if response := mustRead(t, transport); response["error"] != nil {
					t.Fatalf("SDK negotiation: %s", response["error"])
				}
				if !modern {
					if err := transport.Write(wire.Notification("notifications/initialized", nil)); err != nil {
						t.Fatal(err)
					}
					select {
					case <-transport.ready:
					case <-time.After(5 * time.Second):
						t.Fatal("SDK initialization did not finish")
					}
				}
				request := rpcRequest("sdk-tool", "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"value": "SDK round trip"}})
				if modern {
					request = modernHTTPMessage("sdk-tool", "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"value": "SDK round trip"}})
				}
				if err := transport.Write(request); err != nil {
					t.Fatal(err)
				}
				response := mustRead(t, transport)
				var result mcp.CallToolResult
				if response["error"] != nil || json.Unmarshal(response["result"], &result) != nil || len(result.Content) != 1 {
					t.Fatalf("SDK tool response: %#v", response)
				}
				text, ok := result.Content[0].(*mcp.TextContent)
				if !ok || text.Text != "SDK round trip" {
					t.Fatalf("SDK content lost: %#v", result.Content)
				}
				closeTransport(t, transport)
			})
		}
	}
}
