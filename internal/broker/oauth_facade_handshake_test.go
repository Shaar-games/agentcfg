package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestOAuthFacadePreservesAccessibleHandshake(t *testing.T) {
	for _, access := range []string{"public", "cached"} {
		for _, protocol := range []string{"legacy", "modern"} {
			for _, sharing := range []string{"shared", "isolated"} {
				t.Run(access+"/"+protocol+"/"+sharing, func(t *testing.T) {
					modern := protocol == "modern"
					fixture := newFacadeHandshakeFixture(t, access == "public")
					root := oauthStdioHome(t, fixture.http.URL+"/mcp", sharing, nil)
					if access == "cached" {
						cfg, err := config.Load(root)
						if err != nil {
							t.Fatal(err)
						}
						server := auth.InteractiveServer(cfg.Servers["fixture"])
						ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
						defer cancel()
						err = auth.Login(ctx, server, auth.Options{Home: root,
							AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
								return fixture.fetch(ctx, server.OAuth.RedirectURL, args)
							}})
						if err != nil {
							t.Fatalf("prepare local cached credentials: %v", err)
						}
					}
					initializes, discovers, signIns := fixture.initializes.Load(), fixture.discovers.Load(), fixture.browserCalls.Load()
					serveOAuthStdioBroker(t, root)
					var presentations atomic.Int32
					unexpectedAuthorization := errors.New("accessible handshake must not request interactive authorization")
					peer := connectOAuthElicitationWithOptions(t, root, modern,
						map[string]any{"elicitation": map[string]any{"url": map[string]any{}}}, ConnectOptions{
							AuthorizationCodeFetcher: func(context.Context, string, *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
								presentations.Add(1)
								return nil, unexpectedAuthorization
							},
							ClientAuthorizationCodeFetcher: func(context.Context, string, *sdkauth.AuthorizationArgs, func(context.Context, string) error) (*sdkauth.AuthorizationResult, error) {
								presentations.Add(1)
								return nil, unexpectedAuthorization
							},
						})
					if modern {
						peer.send(t, "server/discover", "handshake", nil)
					} else {
						peer.send(t, "initialize", "handshake", map[string]any{
							"protocolVersion": "2025-11-25", "capabilities": peer.capabilities,
							"clientInfo": map[string]string{"name": "metadata-fixture-client", "version": "1"},
						})
					}
					response := peer.response(t, "handshake")
					var got map[string]any
					if err := json.Unmarshal(response["result"], &got); err != nil {
						t.Fatal(err)
					}
					want := facadeHandshakeMetadata(modern)
					if !modern && sharing == "shared" {
						// Preserve the pre-existing shared-backend policy only: session
						// logging and resource subscriptions have no single client owner.
						capabilities := want["capabilities"].(map[string]any)
						delete(capabilities, "logging")
						delete(capabilities["resources"].(map[string]any), "subscribe")
					}
					// Compare JSON values, including nested and unknown capability data.
					var expected map[string]any
					if err := json.Unmarshal(wire.Raw(want), &expected); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, expected) {
						t.Errorf("accessible upstream handshake was replaced or changed:\n got %s\nwant %s", wire.Raw(got), wire.Raw(expected))
					}
					if modern && fixture.discovers.Load() != discovers+1 || !modern && fixture.initializes.Load() != initializes+1 {
						t.Errorf("handshake must reach upstream exactly once: initializes=%d discovers=%d", fixture.initializes.Load()-initializes, fixture.discovers.Load()-discovers)
					}
					if presentations.Load() != 0 || fixture.browserCalls.Load() != signIns {
						t.Errorf("accessible upstream prompted for login: presentations=%d sign-ins=%d", presentations.Load(), fixture.browserCalls.Load()-signIns)
					}
				})
			}
		}
	}
}

func facadeHandshakeMetadata(modern bool) map[string]any {
	capabilities := map[string]any{
		"tools": map[string]any{"listChanged": true}, "prompts": map[string]any{"listChanged": true},
		"resources": map[string]any{"listChanged": true, "subscribe": true},
		"logging":   map[string]any{}, "completions": map[string]any{},
		"experimental": map[string]any{"fixture/custom": map[string]any{"retained": true}},
	}
	result := map[string]any{
		"serverInfo":   map[string]any{"name": "real-handshake-fixture", "version": "3.2.1"},
		"instructions": "Keep the upstream instructions visible to the MCP client.",
		"capabilities": capabilities,
	}
	if modern {
		result["supportedVersions"] = []string{wire.ModernProtocolVersion}
		result["resultType"] = "complete"
	} else {
		// The upstream may negotiate an older version than the client's offer.
		result["protocolVersion"] = "2025-03-26"
	}
	return result
}

func newFacadeHandshakeFixture(t *testing.T, public bool) *oauthStdioFixture {
	t.Helper()
	fixture := &oauthStdioFixture{t: t, grants: make(map[string]oauthStdioGrant)}
	fixture.public.Store(public)
	fixture.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reuse the local provider's authorization/PKCE/token behavior, replacing
		// only successful handshake metadata. No personal credentials are used.
		recorded := httptest.NewRecorder()
		fixture.serve(recorded, r)
		body := recorded.Body.Bytes()
		if r.URL.Path == "/mcp" && recorded.Code == http.StatusOK {
			var response wire.Message
			var result map[string]json.RawMessage
			if json.Unmarshal(body, &response) == nil && json.Unmarshal(response["result"], &result) == nil {
				if result["protocolVersion"] != nil || result["supportedVersions"] != nil {
					response["result"] = wire.Raw(facadeHandshakeMetadata(result["supportedVersions"] != nil))
					body = wire.Raw(response)
				}
			}
		}
		for name, values := range recorded.Header() {
			w.Header()[name] = append([]string(nil), values...)
		}
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(body)
	}))
	t.Cleanup(fixture.http.Close)
	return fixture
}
