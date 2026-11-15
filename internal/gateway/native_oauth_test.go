package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

type nativeProvider struct {
	t            *testing.T
	server       *httptest.Server
	mu           sync.Mutex
	codes        map[string]url.Values
	access       string
	scopes       string
	redirect     string
	pkce         string
	requireWrite atomic.Bool
	denied       atomic.Int32
	logins       atomic.Int32
	exchanges    atomic.Int32
	tools        atomic.Int32
}

func newNativeProvider(t *testing.T) *nativeProvider {
	t.Helper()
	f := &nativeProvider{t: t, codes: make(map[string]url.Values)}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *nativeProvider) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resource := f.server.URL + "/mcp"
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource":
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": resource, "authorization_servers": []string{f.server.URL}, "scopes_supported": []string{"tools.read"}})
	case "/.well-known/oauth-authorization-server":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.server.URL, "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token",
			"registration_endpoint": f.server.URL + "/register", "response_types_supported": []string{"code"},
			"grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"}, "authorization_response_iss_parameter_supported": true,
			"scopes_supported": []string{"tools.read", "tools.write", "offline_access"},
		})
	case "/register":
		var metadata map[string]any
		if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil || metadata["token_endpoint_auth_method"] != "none" {
			http.Error(w, "invalid registration", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "upstream-native-fixture", "redirect_uris": metadata["redirect_uris"], "token_endpoint_auth_method": "none"})
	case "/authorize":
		query := r.URL.Query()
		if query.Get("client_id") != "upstream-native-fixture" || query.Get("resource") != resource || query.Get("code_challenge_method") != "S256" || query.Get("state") == "" {
			f.t.Error("upstream authorization was not a separate PKCE flow")
			http.Error(w, "invalid authorization", http.StatusBadRequest)
			return
		}
		code := fmt.Sprintf("upstream-code-private-%d", f.logins.Add(1))
		f.mu.Lock()
		f.codes[code] = query
		f.redirect, f.pkce = query.Get("redirect_uri"), query.Get("code_challenge")
		f.mu.Unlock()
		callback, _ := url.Parse(query.Get("redirect_uri"))
		callback.RawQuery = url.Values{"code": {code}, "state": {query.Get("state")}, "iss": {f.server.URL}}.Encode()
		http.Redirect(w, r, callback.String(), http.StatusFound)
	case "/token":
		if r.ParseForm() != nil {
			http.Error(w, "invalid token request", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		grant := f.codes[r.Form.Get("code")]
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("grant_type") != "authorization_code" ||
			grant == nil || grant.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) ||
			r.Form.Get("redirect_uri") != grant.Get("redirect_uri") || r.Form.Get("resource") != resource {
			f.mu.Unlock()
			f.t.Error("upstream code exchange lost PKCE, resource, or redirect binding")
			http.Error(w, "invalid grant", http.StatusBadRequest)
			return
		}
		delete(f.codes, r.Form.Get("code"))
		f.access = fmt.Sprintf("upstream-token-private-%d", f.exchanges.Add(1))
		f.scopes = grant.Get("scope")
		access, scopes := f.access, f.scopes
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": "upstream-refresh-private", "expires_in": 3600, "token_type": "Bearer", "scope": scopes})
	case "/mcp":
		f.mu.Lock()
		access, scopes := f.access, f.scopes
		f.mu.Unlock()
		if bearer := r.Header.Get("Authorization"); access == "" || bearer != "Bearer "+access {
			if bearer != "" && !strings.HasPrefix(bearer, "Bearer upstream-token-private-") {
				f.t.Error("a downstream bearer was sent to the upstream server")
			}
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q, scope="tools.read"`, f.server.URL+"/.well-known/oauth-protected-resource/mcp"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var message wire.Message
		if json.NewDecoder(r.Body).Decode(&message) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !message.HasID() {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result wire.Message
		switch message.Method() {
		case "server/discover":
			result = wire.Error(message.ID(), -32601, "legacy fixture")
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "upstream-session-private")
			result = wire.Result(message.ID(), map[string]any{"protocolVersion": "2025-11-25", "serverInfo": map[string]any{"name": "native-provider", "version": "1"}, "capabilities": map[string]any{"tools": map[string]any{}}})
		case "tools/list":
			result = wire.Result(message.ID(), map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "Fixture echo", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}}}})
		case "tools/call":
			if f.requireWrite.Load() && !strings.Contains(" "+scopes+" ", " tools.write ") {
				f.denied.Add(1)
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="insufficient_scope", scope="tools.write", resource_metadata=%q`, f.server.URL+"/.well-known/oauth-protected-resource/mcp"))
				w.WriteHeader(http.StatusForbidden)
				return
			}
			f.tools.Add(1)
			result = wire.Result(message.ID(), map[string]any{"content": []any{map[string]string{"type": "text", "text": "native OAuth worked"}}})
		default:
			result = wire.Error(message.ID(), -32601, "method not supported")
		}
		_ = json.NewEncoder(w).Encode(result)
	default:
		http.NotFound(w, r)
	}
}

func unusedNativeCallback(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	result := "http://" + listener.Addr().String() + "/callback"
	_ = listener.Close()
	return result
}

// This uses the unmodified Go MCP SDK's HTTP client and OAuth handler. No
// elicitation capability or agentcfg authentication tool participates.
func TestNativeOAuthStandardSDKEndToEnd(t *testing.T) {
	testNativeOAuthClient(t, false)
}

func TestNativeOAuthScopeConsentRequiresNewLegacySession(t *testing.T) {
	testNativeOAuthClient(t, true)
}

type nativeStatusWriter struct {
	http.ResponseWriter
	record func(int)
	wrote  bool
}

func (w *nativeStatusWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote = true
		w.record(status)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *nativeStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func testNativeOAuthClient(t *testing.T, stepUp bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	provider := newNativeProvider(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agentcfg"), 0o700); err != nil {
		t.Fatal(err)
	}
	front := httptest.NewUnstartedServer(nil)
	address := front.Listener.Addr().String()
	base := "http://" + address
	settings := config.Settings{Harnesses: []string{"cursor"}, Gateway: config.GatewaySettings{Enabled: true, Listen: address}}
	entry := config.Server{Type: "streamable-http", URL: provider.server.URL + "/mcp", OAuth: &config.OAuthConfig{DynamicRegistration: true, RedirectURL: unusedNativeCallback(t)}}
	for name, value := range map[string]any{"agentcfg.json": settings, "mcp.json": map[string]any{"mcpServers": map[string]any{"fixture": entry}}} {
		data, err := json.Marshal(value)
		if err != nil || os.WriteFile(filepath.Join(root, ".agentcfg", name), data, 0o600) != nil {
			t.Fatal("write isolated gateway configuration")
		}
	}
	store, err := auth.NewEncryptedFileStore(root, bytes.Repeat([]byte{31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(ctx, root, Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	settings.Gateway.RouteKey, err = auth.GatewayKey(ctx, root, store, false)
	if err != nil {
		t.Fatal(err)
	}
	var localBearer atomic.Value
	var sessionMissing atomic.Int32
	front.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/mcp/") && r.Header.Get("Authorization") != "" {
			localBearer.Store(r.Header.Get("Authorization"))
		}
		tracked := &nativeStatusWriter{ResponseWriter: w, record: func(status int) {
			if status == http.StatusNotFound && r.Header.Get("Mcp-Session-Id") != "" {
				sessionMissing.Add(1)
			}
		}}
		handler.ServeHTTP(tracked, r)
	})
	front.Start()
	t.Cleanup(front.Close)
	resource := config.GatewayResourceURL(settings.Gateway, "fixture", "cursor")

	callbackURL := make(chan string, 1)
	clientCallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callbackURL <- "http://" + r.Host + r.URL.RequestURI()
		_, _ = io.WriteString(w, "client received its own authorization code")
	}))
	t.Cleanup(clientCallback.Close)
	redirect := clientCallback.URL + "/callback"
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	var presentations atomic.Int32
	var clientPKCE atomic.Value
	clientOAuth, err := sdkauth.NewAuthorizationCodeHandler(&sdkauth.AuthorizationCodeHandlerConfig{
		RedirectURL: redirect, Client: &http.Client{Timeout: 20 * time.Second}, RequestRefreshToken: true,
		DynamicClientRegistrationConfig: &sdkauth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: "Standard SDK client", RedirectURIs: []string{redirect}, TokenEndpointAuthMethod: "none",
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
		}},
		AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
			presentations.Add(1)
			target, err := url.Parse(args.URL)
			if err != nil || target.Scheme+"://"+target.Host != base || target.Path != "/oauth/authorize" {
				return nil, fmt.Errorf("client was not directed to agentcfg's authorization endpoint")
			}
			clientPKCE.Store(target.Query().Get("code_challenge"))
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
			response, err := browser.Do(request)
			if err != nil {
				return nil, err
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != 200 {
				return nil, fmt.Errorf("agentcfg consent page failed: status %d", response.StatusCode)
			}
			values := url.Values{"decision": {"allow"}}
			for _, field := range regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`).FindAllSubmatch(body, -1) {
				values.Set(string(field[1]), html.UnescapeString(string(field[2])))
			}
			if values.Get("ticket") == "" || values.Get("csrf") == "" {
				return nil, fmt.Errorf("consent page did not bind the client transaction")
			}
			request, _ = http.NewRequestWithContext(ctx, http.MethodPost, base+"/oauth/authorize", strings.NewReader(values.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", base)
			response, err = browser.Do(request)
			if err != nil {
				return nil, err
			}
			body, err = io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK || response.Request.URL.Path != "/oauth/authorize" ||
				!strings.Contains(response.Header.Get("Content-Security-Policy"), "form-action 'self'") || response.Header.Get("Referrer-Policy") != "no-referrer" {
				return nil, fmt.Errorf("consent did not commit its protected navigation document: status %d", response.StatusCode)
			}
			link := regexp.MustCompile(`<a id="oauth-continue" href="([^"]+)">`).FindSubmatch(body)
			refresh := regexp.MustCompile(`<meta http-equiv="refresh" content="0;url=([^"]+)">`).FindSubmatch(body)
			if len(link) != 2 || len(refresh) != 2 || html.UnescapeString(string(link[1])) != html.UnescapeString(string(refresh[1])) {
				return nil, fmt.Errorf("consent document did not offer an automatic browser continuation")
			}
			// net/http does not perform HTML navigation. Start a fresh GET, as the
			// committed document's meta refresh (or explicit link) does in a browser.
			request, err = http.NewRequestWithContext(ctx, http.MethodGet, html.UnescapeString(string(link[1])), nil)
			if err != nil {
				return nil, err
			}
			response, err = browser.Do(request)
			if err != nil {
				return nil, err
			}
			body, err = io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("browser continuation failed: status %d", response.StatusCode)
			}
			select {
			case callback := <-callbackURL:
				if strings.Contains(callback, "upstream-code-private") || strings.Contains(callback, "upstream-token-private") {
					return nil, fmt.Errorf("upstream credentials reached the client callback")
				}
				return auth.ParseAuthorizationResponse(redirect, callback)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "native-oauth-standard-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: resource, HTTPClient: &http.Client{Timeout: 25 * time.Second}, OAuthHandler: clientOAuth}, nil)
	if err != nil {
		t.Fatalf("native OAuth SDK connection: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	catalog, err := session.ListTools(ctx, nil)
	if err != nil || len(catalog.Tools) != 1 || catalog.Tools[0].Name != "echo" {
		t.Fatalf("real catalog was not exposed directly: catalog=%v err=%v", catalog, err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"value": "native"}})
	if err != nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("native tool call failed: result=%v err=%v", result, err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "native OAuth worked" {
		t.Fatalf("unexpected tool result: %v", result)
	}
	provider.mu.Lock()
	upstreamRedirect, upstreamPKCE := provider.redirect, provider.pkce
	provider.mu.Unlock()
	if presentations.Load() != 1 || provider.logins.Load() != 1 || provider.exchanges.Load() != 1 || provider.tools.Load() != 1 {
		t.Fatalf("unexpected auth/tool executions: client=%d provider=%d exchanges=%d tools=%d", presentations.Load(), provider.logins.Load(), provider.exchanges.Load(), provider.tools.Load())
	}
	if upstreamRedirect == redirect || upstreamRedirect != entry.OAuth.RedirectURL || upstreamPKCE == "" || upstreamPKCE == clientPKCE.Load().(string) {
		t.Fatal("the two OAuth legs did not keep independent callbacks and PKCE")
	}
	if bearer, _ := localBearer.Load().(string); bearer == "" || strings.Contains(bearer, "upstream-") {
		t.Fatal("client did not use its own agentcfg bearer")
	}
	t.Log("Standard MCP HTTP client completed its native OAuth flow; upstream PKCE/callback/token stayed separate, and tools/list exposed only the actual upstream tool.")
	if !stepUp {
		return
	}

	// A new provider permission requires a new local grant. The old legacy
	// session remains bound to its original grant and must not migrate silently.
	provider.requireWrite.Store(true)
	beforePKCE := clientPKCE.Load().(string)
	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"value": "requires-write"}})
	if err == nil || sessionMissing.Load() != 1 {
		t.Fatalf("new grant did not terminate the old session: err=%v gateway404=%d", err, sessionMissing.Load())
	}
	if provider.denied.Load() != 1 || provider.tools.Load() != 1 {
		t.Fatalf("scope denial executed or replayed the tool: rejected=%d executed=%d", provider.denied.Load(), provider.tools.Load())
	}
	if presentations.Load() != 2 || provider.logins.Load() != 2 || provider.exchanges.Load() != 2 || clientPKCE.Load().(string) == beforePKCE {
		t.Fatalf("scope consent was not one separate native OAuth flow: client=%d provider=%d exchanges=%d", presentations.Load(), provider.logins.Load(), provider.exchanges.Load())
	}
	if _, err := session.ListTools(ctx, nil); err == nil {
		t.Fatal("SDK continued using the terminal legacy session")
	}
	_ = session.Close()
	reconnected, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: resource, HTTPClient: &http.Client{Timeout: 25 * time.Second}, OAuthHandler: clientOAuth}, nil)
	if err != nil {
		t.Fatalf("explicit reconnect with the acquired grant: %v", err)
	}
	t.Cleanup(func() { _ = reconnected.Close() })
	result, err = reconnected.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"value": "explicit-retry"}})
	if err != nil || result.IsError {
		t.Fatalf("explicit call after reconnect: result=%v err=%v", result, err)
	}
	if presentations.Load() != 2 || provider.logins.Load() != 2 || provider.exchanges.Load() != 2 || provider.tools.Load() != 2 || provider.denied.Load() != 1 {
		t.Fatalf("reconnection repeated authorization or tool execution: client=%d provider=%d exchanges=%d tools=%d denied=%d", presentations.Load(), provider.logins.Load(), provider.exchanges.Load(), provider.tools.Load(), provider.denied.Load())
	}
	if bearer, _ := localBearer.Load().(string); bearer == "" || strings.Contains(bearer, "upstream-") {
		t.Fatal("upstream bearer reached the client during reconnection")
	}
	t.Log("Provider insufficient_scope triggered one native OAuth step-up; the previous grant's session ended with 404, then an explicit reconnect reused the new credentials without another login or an uncertain tool replay.")
}
