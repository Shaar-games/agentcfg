package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"golang.org/x/oauth2"
)

type authDiagnosticTokenSource func() (*oauth2.Token, error)

func (f authDiagnosticTokenSource) Token() (*oauth2.Token, error) { return f() }

// The SDK can fail while obtaining its token source, obtaining a token from
// that source, or handling an HTTP challenge. Exercise the public JSON-RPC
// response so retaining an internal error alone cannot satisfy this regression.
func TestHTTPOAuthDiagnostics(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(fmt.Sprintf("modern=%t", modern), func(t *testing.T) {
			for _, tc := range []struct {
				name        string
				mode        string
				status      int
				staticAuth  bool
				wantMessage []string
				wantPosts   int32
				wantFlows   int32
			}{
				{name: "no-oauth-401", status: 401, wantPosts: 1,
					wantMessage: []string{"HTTP 401", "check this server's authentication configuration in agentcfg", "the operation is not retried"}},
				{name: "static-header-rejected", status: 401, staticAuth: true, wantPosts: 1,
					wantMessage: []string{"HTTP 401", "check this server's authentication configuration in agentcfg", "the operation is not retried"}},
				{name: "token-source-construction", mode: "source-error", status: 401,
					wantMessage: []string{"MCP OAuth token acquisition failed", "agentcfg auth status <server>", "the operation is not retried"}},
				{name: "token-acquisition", mode: "token-error", status: 401,
					wantMessage: []string{"MCP OAuth token acquisition failed", "agentcfg auth status <server>", "the operation is not retried"}},
				{name: "token-login-required", mode: "token-login", status: 401,
					wantMessage: []string{"OAuth login required"}},
				{name: "invalid-grant-challenge", mode: "invalid-grant", status: 401, wantPosts: 1, wantFlows: 1,
					wantMessage: []string{"OAuth login required"}},
				{name: "authorize-401", mode: "authorize-error", status: 401, wantPosts: 1, wantFlows: 1,
					wantMessage: []string{"MCP OAuth authorization failed", "agentcfg auth login <server>", "the operation is not retried"}},
				{name: "authorize-403", mode: "authorize-error", status: 403, wantPosts: 1, wantFlows: 1,
					wantMessage: []string{"MCP OAuth authorization failed", "agentcfg auth login <server>", "the operation is not retried"}},
				{name: "challenge-login-required-401", mode: "authorize-login", status: 401, wantPosts: 1, wantFlows: 1,
					wantMessage: []string{"OAuth login required"}},
				{name: "challenge-login-required-403", mode: "authorize-login", status: 403, wantPosts: 1, wantFlows: 1,
					wantMessage: []string{"OAuth login required"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var posts, flows atomic.Int32
					transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != http.MethodPost {
							t.Errorf("unexpected diagnostic fixture method %s", r.Method)
							w.WriteHeader(http.StatusMethodNotAllowed)
							return
						}
						posts.Add(1)
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("WWW-Authenticate", `Bearer error_description="provider-challenge-secret", resource_metadata="https://auth.example.invalid/meta?key=provider-metadata-secret"`)
						w.Header().Set("Mcp-Session-Id", "provider-session-secret")
						w.WriteHeader(tc.status)
						_, _ = w.Write([]byte(`{"error":"provider-body-secret"}`))
					}))
					if !tc.staticAuth {
						transport.headers.Del("Authorization")
					}
					if tc.mode != "" {
						transport.oauth = testOAuthHandler{
							tokenSource: func(context.Context) (oauth2.TokenSource, error) {
								switch tc.mode {
								case "source-error":
									return nil, errors.New("token-source-secret at https://auth.example.invalid/token?code=private-code")
								case "token-error":
									return authDiagnosticTokenSource(func() (*oauth2.Token, error) {
										return nil, errors.New("token-source-secret at https://auth.example.invalid/token?code=private-code")
									}), nil
								case "token-login":
									return authDiagnosticTokenSource(func() (*oauth2.Token, error) {
										return nil, fmt.Errorf("token-source-secret: %w", auth.ErrLoginRequired)
									}), nil
								case "invalid-grant":
									return authDiagnosticTokenSource(func() (*oauth2.Token, error) {
										return nil, &oauth2.RetrieveError{ErrorCode: "invalid_grant", ErrorDescription: "provider-refresh-secret",
											Response: &http.Response{StatusCode: http.StatusBadRequest}, Body: []byte("provider-token-body-secret")}
									}), nil
								default:
									return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fixture-token-secret", TokenType: "Bearer"}), nil
								}
							},
							authorize: func(context.Context, *http.Request, *http.Response) error {
								flows.Add(1)
								if tc.mode == "authorize-login" || tc.mode == "invalid-grant" {
									return fmt.Errorf("authz-secret: %w", auth.ErrLoginRequired)
								}
								return errors.New("authz-secret at https://auth.example.invalid/authorize?code=private-code")
							},
						}
					}
					request := rpcRequest("diagnostic-request", "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})
					if modern {
						request = modernHTTPMessage("diagnostic-request", "server/discover", nil)
					}
					if err := transport.Write(request); err != nil {
						t.Fatal(err)
					}
					response := mustRead(t, transport)
					closeTransport(t, transport)
					if string(response.ID()) != string(request.ID()) {
						t.Errorf("response ID = %s, want %s", response.ID(), request.ID())
					}
					var rpcError struct {
						Code    int    `json:"code"`
						Message string `json:"message"`
					}
					if err := json.Unmarshal(response["error"], &rpcError); err != nil {
						t.Fatalf("invalid JSON-RPC error: %v", err)
					}
					if rpcError.Code != -32000 {
						t.Errorf("error code = %d, want -32000", rpcError.Code)
					}
					for _, want := range tc.wantMessage {
						if !strings.Contains(rpcError.Message, want) {
							t.Errorf("message %q does not contain %q", rpcError.Message, want)
						}
					}
					encoded, err := json.Marshal(response)
					if err != nil {
						t.Fatal(err)
					}
					for _, secret := range []string{"url-secret", "header-secret", "provider-challenge-secret", "provider-metadata-secret", "provider-session-secret", "provider-body-secret", "provider-refresh-secret", "provider-token-body-secret", "token-source-secret", "authz-secret", "fixture-token-secret", "private-code", "auth.example.invalid", "127.0.0.1"} {
						if strings.Contains(string(encoded), secret) {
							t.Errorf("diagnostic exposed fixture secret %q", secret)
						}
					}
					if posts.Load() != tc.wantPosts || flows.Load() != tc.wantFlows {
						t.Errorf("POST/Authorize calls = %d/%d, want %d/%d; diagnostic handling must not replay requests", posts.Load(), flows.Load(), tc.wantPosts, tc.wantFlows)
					}
				})
			}
		})
	}
}

// A login-required result can race with request cancellation. Synchronize that
// ordering explicitly: cancellation remains the useful diagnosis once the
// operation's context has ended, regardless of the OAuth error's identity.
func TestHTTPOAuthCancelledLoginDiagnostic(t *testing.T) {
	for _, phase := range []string{"token", "authorize"} {
		t.Run(phase, func(t *testing.T) {
			entered := make(chan struct{})
			var posts atomic.Int32
			transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"cancel-private-secret"}`))
			}))
			transport.headers.Del("Authorization")
			waitForCancellation := func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				return fmt.Errorf("cancel-private-secret: %w", auth.ErrLoginRequired)
			}
			transport.oauth = testOAuthHandler{
				tokenSource: func(ctx context.Context) (oauth2.TokenSource, error) {
					if phase == "token" {
						return authDiagnosticTokenSource(func() (*oauth2.Token, error) {
							return nil, waitForCancellation(ctx)
						}), nil
					}
					return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "cancel-private-secret", TokenType: "Bearer"}), nil
				},
				authorize: func(ctx context.Context, _ *http.Request, _ *http.Response) error {
					return waitForCancellation(ctx)
				},
			}
			request := modernHTTPMessage("cancelled-oauth", "tools/list", nil)
			if err := transport.Write(request); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("OAuth operation did not reach its cancellation barrier")
			}
			cancelled := make(chan struct{})
			go func() {
				transport.CancelRequest(request.ID())
				close(cancelled)
			}()
			select {
			case <-cancelled:
			case <-time.After(5 * time.Second):
				t.Fatal("OAuth request cancellation did not drain")
			}
			response := mustRead(t, transport)
			closeTransport(t, transport)
			if string(response.ID()) != string(request.ID()) {
				t.Errorf("response ID = %s, want %s", response.ID(), request.ID())
			}
			var rpcError struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(response["error"], &rpcError); err != nil {
				t.Fatal(err)
			}
			if rpcError.Code != -32000 || !strings.Contains(rpcError.Message, "cancelled") || strings.Contains(rpcError.Message, "login") {
				t.Errorf("cancelled OAuth request should report cancellation, got %d: %s", rpcError.Code, rpcError.Message)
			}
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"cancel-private-secret", "url-secret", "header-secret"} {
				if strings.Contains(string(encoded), secret) {
					t.Errorf("cancellation diagnostic exposed fixture secret %q", secret)
				}
			}
			wantPosts := int32(0)
			if phase == "authorize" {
				wantPosts = 1
			}
			if posts.Load() != wantPosts {
				t.Errorf("POST calls = %d, want %d; cancellation must not replay the request", posts.Load(), wantPosts)
			}
		})
	}
}
