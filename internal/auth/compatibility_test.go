package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"agentcfg/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestSDKDynamicRegistrationAcceptsHTTPSLoopbackRedirect(t *testing.T) {
	for _, redirect := range []string{
		"http://127.0.0.1:48761/callback",
		"https://127.0.0.1:48761/callback",
		"https://localhost/callback",
		"https://[::1]/callback",
		"https://client.example.test/callback",
	} {
		t.Run(redirect, func(t *testing.T) {
			server := config.Server{Type: "streamable-http", URL: "https://mcp.example.test/mcp",
				OAuth: &config.OAuthConfig{DynamicRegistration: true, RedirectURL: redirect}}
			if _, err := NewHandler(context.Background(), server, Options{Store: testStore(t)}); err != nil {
				t.Fatalf("valid DCR callback rejected: %v", err)
			}
		})
	}
}

func TestSDKAuthorizationUsesExplicitChallengeScopes(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newOAuthFixture(t)
			f.allowWrite = true
			var requested []string
			fetch := func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
				u, err := url.Parse(args.URL)
				if err != nil {
					return nil, err
				}
				requested = strings.Fields(u.Query().Get("scope"))
				return f.fetch(ctx, args)
			}
			h, err := NewHandler(context.Background(), f.server, Options{Store: testStore(t), AuthorizationCodeFetcher: fetch})
			if err != nil {
				t.Fatal(err)
			}
			request, _ := http.NewRequest(http.MethodGet, f.server.URL, nil)
			response := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}
			response.Header.Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q, scope="tools.write", error="insufficient_scope"`, f.http.URL+"/.well-known/oauth-protected-resource/mcp"))
			if err := h.Authorize(context.Background(), request, response); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(requested, "tools.read") || !slices.Contains(requested, "tools.write") {
				t.Fatalf("explicit settings discarded the challenged scope: %v", requested)
			}
		})
	}
}

func TestSDKDynamicRegistrationValidatesUnadvertisedIssuer(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("mismatch=%t", mismatch), func(t *testing.T) {
			f := newOAuthFixture(t)
			f.omitIssuerHint = true
			f.server.OAuth.ClientID = ""
			f.server.OAuth.Issuer = ""
			f.server.OAuth.DynamicRegistration = true
			store := testStore(t)
			fetch := func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
				result, err := f.fetch(ctx, args)
				if err == nil && mismatch {
					result.Iss = "https://wrong-issuer.example.test"
				}
				return result, err
			}
			err := Login(context.Background(), f.server, Options{Store: store, AuthorizationCodeFetcher: fetch})
			if mismatch {
				if err == nil {
					t.Fatal("login accepted a callback from another issuer")
				}
				state, inspectErr := Inspect(context.Background(), f.server, Options{Store: store})
				if inspectErr != nil || state.Stored {
					t.Fatalf("issuer mismatch persisted credentials: %+v, %v", state, inspectErr)
				}
			} else if err != nil {
				t.Fatalf("valid issuer rejected because metadata did not advertise iss support: %v", err)
			}
		})
	}
}
