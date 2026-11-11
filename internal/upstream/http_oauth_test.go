package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Exercise the production encrypted store and HTTP transport together. A
// synthetic token source alone cannot detect credentials lost between the
// explicit login command and an already running broker transport.
func TestHTTPOAuthLoginStoreAndSDKTransport(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, jsonResponse := range []bool{false, true} {
			t.Run(fmt.Sprintf("modern=%t/json=%t", modern, jsonResponse), func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("USERPROFILE", home)
				t.Setenv("AGENTCFG_OAUTH_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x75}, 32)))
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				sdkServer := mcp.NewServer(&mcp.Implementation{Name: "authenticated-fixture", Version: "1"}, nil)
				sdkServer.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
					func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "authenticated round trip"}}}, nil
					})
				mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdkServer },
					&mcp.StreamableHTTPOptions{Stateless: modern, JSONResponse: jsonResponse, PropagateRequestCancellation: true})
				var fixture *httptest.Server
				var authorizations, exchanges atomic.Int32
				var challenge atomic.Value
				fixture = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/mcp" {
						if r.Header.Get("Authorization") != "Bearer stored-access" {
							w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, fixture.URL+"/.well-known/oauth-protected-resource/mcp"))
							w.WriteHeader(http.StatusUnauthorized)
							return
						}
						if r.Header.Get("X-MCP-Only") != "resource-only" {
							t.Error("authenticated MCP request lost configured headers")
						}
						mcpHandler.ServeHTTP(w, r)
						return
					}
					if r.Header.Get("X-MCP-Only") != "" || r.Header.Get("Authorization") != "" {
						t.Error("resource credentials leaked into OAuth discovery or exchange")
					}
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/.well-known/oauth-protected-resource/mcp":
						_ = json.NewEncoder(w).Encode(map[string]any{"resource": fixture.URL + "/mcp", "authorization_servers": []string{fixture.URL}})
					case "/.well-known/oauth-authorization-server":
						_ = json.NewEncoder(w).Encode(map[string]any{
							"issuer": fixture.URL, "authorization_endpoint": fixture.URL + "/authorize", "token_endpoint": fixture.URL + "/token",
							"response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"},
							"token_endpoint_auth_methods_supported": []string{"client_secret_post"},
						})
					case "/token":
						if err := r.ParseForm(); err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
						if r.Form.Get("resource") != fixture.URL+"/mcp" || r.Form.Get("grant_type") != "authorization_code" ||
							base64.RawURLEncoding.EncodeToString(sum[:]) != challenge.Load() {
							t.Error("OAuth exchange lost PKCE or resource binding")
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						exchanges.Add(1)
						_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "stored-access", "token_type": "Bearer", "expires_in": 3600})
					default:
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(fixture.Close)
				server := config.Server{Name: "authenticated", Source: "temporary-config", Type: "streamable-http", URL: fixture.URL + "/mcp",
					Headers: map[string]string{
						"X-MCP-Only": "resource-only", "Accept": "configured-media", "Content-Type": "configured-media",
						"Mcp-Session-Id": "stale-session", "MCP-Protocol-Version": "stale-version", "Mcp-Method": "stale-method",
					},
					OAuth: &config.OAuthConfig{ClientID: "fixture-client", Issuer: fixture.URL, RedirectURL: "http://127.0.0.1:48761/callback"}}
				fetch := func(_ context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
					authorizations.Add(1)
					u, err := url.Parse(args.URL)
					if err != nil {
						return nil, err
					}
					challenge.Store(u.Query().Get("code_challenge"))
					return &sdkauth.AuthorizationResult{Code: "fixture-code", State: u.Query().Get("state"), Iss: fixture.URL}, nil
				}
				transport := startTestTransport(t, ctx, server).(*httpTransport)
				if err := transport.Write(modernHTTPMessage("before-login", "server/discover", nil)); err != nil {
					t.Fatal(err)
				}
				if response := mustRead(t, transport); !strings.Contains(string(response["error"]), "OAuth login required") {
					t.Fatalf("unauthenticated discovery did not require login: %s", response["error"])
				}
				if err := auth.Login(ctx, server, auth.Options{Home: home, AuthorizationCodeFetcher: fetch}); err != nil {
					t.Fatal(err)
				}
				if !modern {
					initializeHTTP(t, transport)
				}
				request := rpcRequest("authenticated-call", "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}})
				if modern {
					request = modernHTTPMessage("authenticated-call", "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}})
				}
				if err := transport.Write(request); err != nil {
					t.Fatal(err)
				}
				response := mustRead(t, transport)
				if response["error"] != nil || !strings.Contains(string(response["result"]), "authenticated round trip") {
					t.Fatalf("transport started before login did not reload credentials: %s", response["error"])
				}
				if authorizations.Load() != 1 || exchanges.Load() != 1 {
					t.Fatalf("new transport repeated interactive authorization: %d authorizations, %d exchanges", authorizations.Load(), exchanges.Load())
				}
				if err := auth.Logout(ctx, server, auth.Options{Home: home}); err != nil {
					t.Fatal(err)
				}
				request["id"] = wire.Raw("after-logout")
				if err := transport.Write(request); err != nil {
					t.Fatal(err)
				}
				if response := mustRead(t, transport); !strings.Contains(string(response["error"]), "OAuth login required") {
					t.Fatalf("existing HTTP transport ignored logout or lost its error: %s", response["error"])
				}
				if err := auth.Login(ctx, server, auth.Options{Home: home, AuthorizationCodeFetcher: fetch}); err != nil {
					t.Fatal(err)
				}
				request["id"] = wire.Raw("after-relogin")
				if err := transport.Write(request); err != nil {
					t.Fatal(err)
				}
				if response := mustRead(t, transport); response["error"] != nil || !strings.Contains(string(response["result"]), "authenticated round trip") {
					t.Fatalf("existing HTTP transport did not recover after login: %s", response["error"])
				}
				closeTransport(t, transport)
			})
		}
	}
}
