package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/clientauth"
	"agentcfg/internal/config"
	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

type proxyOAuthCode struct{ challenge, scopes string }
type proxyOAuthFixture struct {
	t                                                   *testing.T
	remote, completion                                  *httptest.Server
	server                                              config.Server
	resource                                            clientauth.Resource
	principal                                           clientauth.Principal
	store                                               auth.Store
	proxy                                               *Proxy
	mu                                                  sync.Mutex
	codes                                               map[string]proxyOAuthCode
	access                                              string
	write                                               bool
	reject                                              bool
	public                                              bool
	grants, refreshes, tools, presentations, challenges atomic.Int32
	lastInsufficient                                    atomic.Bool
}

func newProxyOAuthFixture(t *testing.T, implicit bool) *proxyOAuthFixture {
	t.Helper()
	f := &proxyOAuthFixture{t: t, codes: make(map[string]proxyOAuthCode), store: proxyTestStore(t)}
	f.remote = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.remote.Close)
	f.completion = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/complete" || r.URL.Query().Get("ticket") != "local-ticket" || r.URL.Query().Has("code") {
			t.Error("upstream callback did not return the local completion ticket")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(f.completion.Close)
	callback, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	redirect := "http://" + callback.Addr().String() + "/upstream-callback"
	_ = callback.Close()
	f.server = config.Server{Name: "fixture", Source: "temporary-config", Type: "streamable-http", URL: f.remote.URL + "/mcp", Headers: map[string]string{"X-Resource-Key": "upstream-only"}}
	if !implicit {
		f.server.OAuth = &config.OAuthConfig{ClientID: "upstream-client", Issuer: f.remote.URL, RedirectURL: redirect, Scopes: []string{"read"}}
	}
	f.resource = clientauth.Resource{URL: f.completion.URL + "/mcp/fixture", Name: "fixture", Identity: "fixture-identity"}
	f.principal = clientauth.Principal{ClientID: "native-client", GrantID: "native-grant", Resource: f.resource.URL}
	f.proxy = NewProxy(ProxyOptions{Store: f.store, HTTPClient: f.remote.Client(), Resolve: func(clientauth.Resource) (config.Server, error) { return f.server, nil },
		Challenge: func(w http.ResponseWriter, r *http.Request, _ clientauth.Resource, _ clientauth.Principal, insufficient bool) {
			f.challenges.Add(1)
			f.lastInsufficient.Store(insufficient)
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="local-only"`)
			w.WriteHeader(401)
		},
	})
	t.Cleanup(func() { _ = f.proxy.Close() })
	return f
}

func (f *proxyOAuthFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		if r.Header.Get("X-Resource-Key") != "" {
			f.t.Error("resource headers leaked to an OAuth endpoint")
		}
		if r.Header.Get("Authorization") != "" {
			client, secret, basic := r.BasicAuth()
			if r.URL.Path != "/token" || !basic || client != "upstream-client" || secret != "" {
				f.t.Error("resource credentials leaked to an OAuth endpoint")
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource":
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": f.server.URL, "authorization_servers": []string{f.remote.URL}, "scopes_supported": []string{"read"}})
	case "/.well-known/oauth-authorization-server":
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.remote.URL, "authorization_endpoint": f.remote.URL + "/authorize", "token_endpoint": f.remote.URL + "/token", "registration_endpoint": f.remote.URL + "/register",
			"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}, "authorization_response_iss_parameter_supported": true})
	case "/register":
		var metadata map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&metadata) != nil {
			http.Error(w, "invalid registration", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "upstream-client", "redirect_uris": metadata["redirect_uris"], "token_endpoint_auth_method": "none"})
	case "/authorize":
		q := r.URL.Query()
		if q.Get("client_id") != "upstream-client" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("resource") != f.server.URL {
			f.t.Error("upstream OAuth parameters are incomplete")
		}
		code := rand.Text()
		f.mu.Lock()
		f.codes[code] = proxyOAuthCode{q.Get("code_challenge"), q.Get("scope")}
		f.mu.Unlock()
		callback, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			http.Error(w, "invalid redirect", 400)
			return
		}
		callback.RawQuery = url.Values{"code": {code}, "state": {q.Get("state")}, "iss": {f.remote.URL}}.Encode()
		http.Redirect(w, r, callback.String(), http.StatusFound)
	case "/token":
		if r.ParseForm() != nil {
			http.Error(w, "bad token request", 400)
			return
		}
		if r.Form.Get("resource") != f.server.URL {
			f.t.Error("upstream token request lost its resource indicator")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		scopes := "read"
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			code, ok := f.codes[r.Form.Get("code")]
			delete(f.codes, r.Form.Get("code"))
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || len(r.Form.Get("code_verifier")) < 43 || base64.RawURLEncoding.EncodeToString(sum[:]) != code.challenge {
				f.t.Error("upstream PKCE verification failed")
				http.Error(w, "bad code", 400)
				return
			}
			scopes = code.scopes
			f.write = strings.Contains(" "+scopes+" ", " write ")
			f.reject = false
			f.access = fmt.Sprintf("upstream-access-%d", f.grants.Add(1))
		case "refresh_token":
			if r.Form.Get("refresh_token") != "upstream-refresh" {
				f.t.Error("wrong upstream refresh token")
			}
			f.access = fmt.Sprintf("upstream-refreshed-%d", f.refreshes.Add(1))
		default:
			http.Error(w, "unsupported grant", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.access, "refresh_token": "upstream-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": scopes})
	case "/mcp":
		f.mu.Lock()
		authorized := f.public || f.access != "" && !f.reject && r.Header.Get("Authorization") == "Bearer "+f.access
		allowedWrite := f.write
		f.mu.Unlock()
		if !authorized {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, f.remote.URL+"/.well-known/oauth-protected-resource/mcp"))
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Mcp-Name") == "write" && !allowedWrite {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="insufficient_scope", scope="write", resource_metadata=%q`, f.remote.URL+"/.well-known/oauth-protected-resource/mcp"))
			w.WriteHeader(403)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		var request wire.Message
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "invalid MCP request", 400)
			return
		}
		if !request.HasID() {
			w.WriteHeader(202)
			return
		}
		var result any = map[string]any{}
		switch request.Method() {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-11-25", "serverInfo": map[string]string{"name": "fixture", "version": "1"}, "capabilities": map[string]any{"tools": map[string]any{}}}
		case "server/discover":
			result = map[string]any{"resultType": "complete", "supportedVersions": []string{wire.ModernProtocolVersion}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}, "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/call":
			f.tools.Add(1)
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "executed"}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		}
		_ = json.NewEncoder(w).Encode(wire.Result(request.ID(), result))
	default:
		http.NotFound(w, r)
	}
}

func (f *proxyOAuthFixture) call(name string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, f.resource.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":{}}}`))
	r.Header.Set("Authorization", "Bearer local-access-token")
	r.Header.Set("Mcp-Name", name)
	w := httptest.NewRecorder()
	f.proxy.ServeMCP(w, r, f.resource, f.principal, f.server)
	return w
}

func (f *proxyOAuthFixture) authorize() error {
	ctx, cancel := context.WithTimeout(f.t.Context(), 10*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	started := false
	err := f.proxy.Authorize(ctx, f.resource, f.principal, func(target string) error {
		f.presentations.Add(1)
		started = true
		go func() {
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			response, err := f.remote.Client().Do(request)
			if err == nil {
				_ = response.Body.Close()
				if response.StatusCode != 204 {
					err = fmt.Errorf("unexpected callback completion status %d", response.StatusCode)
				}
			}
			completed <- err
		}()
		return nil
	}, f.completion.URL+"/oauth/complete?ticket=local-ticket")
	if started {
		select {
		case browserErr := <-completed:
			if err == nil {
				err = browserErr
			}
		case <-ctx.Done():
			if err == nil {
				err = ctx.Err()
			}
		}
	}
	return err
}

func TestProxyOAuthUsesRealCallbackPKCEAndPrivateUpstreamTokens(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "url-only"}[implicit], func(t *testing.T) {
			f := newProxyOAuthFixture(t, implicit)
			before, _ := json.Marshal(f.server)
			if got := f.call("echo"); got.Code != 401 {
				t.Fatalf("missing upstream grant was not a local401: %d %s", got.Code, got.Body.String())
			}
			if f.presentations.Load() != 0 || f.tools.Load() != 0 {
				t.Fatal("ordinary MCP traffic opened OAuth or executed a tool")
			}
			if err := f.authorize(); err != nil {
				t.Fatal(err)
			}
			if f.grants.Load() != 1 || f.presentations.Load() != 1 {
				t.Fatal("real upstream grant did not complete")
			}
			status, err := auth.Inspect(t.Context(), auth.InteractiveServer(f.server), auth.Options{Store: f.store})
			if err != nil || !status.Authenticated {
				t.Fatalf("grant was not persisted under the original identity: %+v %v", status, err)
			}
			got := f.call("echo")
			if got.Code != 200 || !strings.Contains(got.Body.String(), "executed") || f.tools.Load() != 1 {
				t.Fatalf("authenticated tool failed: %d %s", got.Code, got.Body.String())
			}
			if strings.Contains(got.Body.String(), "upstream-access") || strings.Contains(fmt.Sprint(got.Header()), "upstream-access") {
				t.Fatal("upstream token escaped")
			}
			if err := f.authorize(); err != nil {
				t.Fatal(err)
			}
			if f.grants.Load() != 1 || f.presentations.Load() != 1 {
				t.Fatal("cached grant caused another consent")
			}
			after, _ := json.Marshal(f.server)
			if string(before) != string(after) {
				t.Fatal("canonical configuration was changed")
			}
		})
	}
}

func TestProxyOAuthRefreshAndScopeConsent(t *testing.T) {
	f := newProxyOAuthFixture(t, false)
	if err := f.authorize(); err != nil {
		t.Fatal(err)
	}
	key := auth.CredentialKey(auth.InteractiveServer(f.server))
	raw, err := f.store.Load(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	_ = json.Unmarshal(raw, &record)
	var token oauth2.Token
	_ = json.Unmarshal(record["token"], &token)
	token.Expiry = time.Now().Add(-time.Minute)
	record["token"], _ = json.Marshal(token)
	raw, _ = json.Marshal(record)
	if err := f.store.Save(t.Context(), key, raw); err != nil {
		t.Fatal(err)
	}
	if got := f.call("echo"); got.Code != 200 {
		t.Fatalf("silent refresh failed: %d %s", got.Code, got.Body.String())
	}
	if f.refreshes.Load() != 1 || f.presentations.Load() != 1 {
		t.Fatal("expired access token did not refresh silently")
	}
	if got := f.call("write"); got.Code != 401 || !f.lastInsufficient.Load() {
		t.Fatalf("scope rejection was not a local OAuth challenge: %d", got.Code)
	}
	if f.tools.Load() != 1 {
		t.Fatal("denied write was executed or replayed")
	}
	// Native clients can perform DCR again while handling a scope challenge.
	// The new local grant must still authorize the shared upstream credentials.
	f.principal.ClientID = "native-client-reregistered"
	f.principal.GrantID = "native-grant-renewed"
	if err := f.authorize(); err != nil {
		t.Fatal(err)
	}
	if f.presentations.Load() != 2 {
		t.Fatal("scope escalation did not ask for new upstream consent")
	}
	if got := f.call("write"); got.Code != 200 || f.tools.Load() != 2 {
		t.Fatalf("scope grant did not authorize the retried call: %d %s", got.Code, got.Body.String())
	}
}

func TestProxyReusesManualLoginAndAllowsPublicUpstream(t *testing.T) {
	t.Run("manual-login", func(t *testing.T) {
		f := newProxyOAuthFixture(t, false)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		err := auth.Login(ctx, auth.InteractiveServer(f.server), auth.Options{Store: f.store, HTTPClient: f.remote.Client(), AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
			client := *f.remote.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
			response, err := client.Do(request)
			if err != nil {
				return nil, err
			}
			defer response.Body.Close()
			return auth.ParseAuthorizationResponse(f.server.OAuth.RedirectURL, response.Header.Get("Location"))
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.authorize(); err != nil {
			t.Fatal(err)
		}
		if f.presentations.Load() != 0 || f.grants.Load() != 1 {
			t.Fatal("manual credentials were not reused")
		}
		if got := f.call("echo"); got.Code != 200 {
			t.Fatal("manual login did not unlock existing proxy")
		}
	})
	t.Run("public", func(t *testing.T) {
		f := newProxyOAuthFixture(t, true)
		f.public = true
		if err := f.authorize(); err != nil {
			t.Fatal(err)
		}
		if f.presentations.Load() != 0 || f.grants.Load() != 0 {
			t.Fatal("public upstream required OAuth")
		}
		if got := f.call("echo"); got.Code != 200 {
			t.Fatal("public upstream was blocked")
		}
	})
}
