package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentmux/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

type oauthFixture struct {
	t              *testing.T
	http           *httptest.Server
	server         config.Server
	challenge      string
	refreshSecret  string
	mu             sync.Mutex
	refreshes      atomic.Int32
	registrations  atomic.Int32
	requests       atomic.Int32
	noAuth         atomic.Bool
	refreshStarted chan struct{}
	refreshBlock   chan struct{}
}

func newOAuthFixture(t *testing.T) *oauthFixture {
	t.Helper()
	f := &oauthFixture{t: t}
	f.http = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.http.Close)
	f.server = config.Server{Name: "test", Source: "temporary-config", Type: "streamable-http", URL: f.http.URL + "/mcp",
		Headers: map[string]string{"X-MCP-Only": "resource-secret"},
		OAuth:   &config.OAuthConfig{Account: "work", ClientID: "test-client", Issuer: f.http.URL, RedirectURL: "http://127.0.0.1:48761/callback", Scopes: []string{"tools.read"}}}
	return f
}

func (f *oauthFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if r.URL.Path != "/mcp" && r.Header.Get("X-MCP-Only") != "" {
		f.t.Error("resource headers leaked into OAuth requests")
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource":
		json.NewEncoder(w).Encode(map[string]any{"resource": f.http.URL + "/mcp", "authorization_servers": []string{f.http.URL}, "scopes_supported": []string{"tools.read", "tools.write"}})
	case "/.well-known/oauth-authorization-server":
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.http.URL, "authorization_endpoint": f.http.URL + "/authorize", "token_endpoint": f.http.URL + "/token",
			"registration_endpoint": f.http.URL + "/register", "response_types_supported": []string{"code"},
			"grant_types_supported":            []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic"},
			"scopes_supported": []string{"tools.read", "offline_access"}, "authorization_response_iss_parameter_supported": true,
		})
	case "/register":
		f.registrations.Add(1)
		var metadata map[string]any
		if json.NewDecoder(r.Body).Decode(&metadata) != nil {
			http.Error(w, "bad registration", 400)
			return
		}
		if metadata["token_endpoint_auth_method"] != "none" {
			f.t.Error("DCR must request a public client")
		}
		if !strings.Contains(fmt.Sprint(metadata["grant_types"]), "refresh_token") {
			f.t.Error("DCR omitted refresh support")
		}
		json.NewEncoder(w).Encode(map[string]any{"client_id": "dynamic-client", "redirect_uris": []string{f.server.OAuth.RedirectURL}, "token_endpoint_auth_method": "none"})
	case "/token":
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", 400)
			return
		}
		refresh := "refresh-0"
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			f.mu.Lock()
			challenge := f.challenge
			f.mu.Unlock()
			verifier := r.Form.Get("code_verifier")
			sum := sha256.Sum256([]byte(verifier))
			if len(verifier) < 43 || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				f.t.Error("SDK did not send matching PKCE verifier")
				http.Error(w, "bad PKCE", 400)
				return
			}
			if r.Form.Get("resource") != f.server.URL {
				f.t.Errorf("missing resource indicator: %v", r.Form)
			}
		case "refresh_token":
			n := f.refreshes.Add(1)
			_, secret, basic := r.BasicAuth()
			if !basic {
				secret = r.Form.Get("client_secret")
			}
			f.mu.Lock()
			f.refreshSecret = secret
			f.mu.Unlock()
			if r.Form.Get("resource") != f.server.URL {
				f.t.Errorf("refresh lost the original resource indicator: %v", r.Form)
			}
			if f.refreshStarted != nil {
				select {
				case f.refreshStarted <- struct{}{}:
				default:
				}
			}
			if f.refreshBlock != nil {
				select {
				case <-f.refreshBlock:
				case <-r.Context().Done():
					return
				}
			}
			if r.Form.Get("refresh_token") != fmt.Sprintf("refresh-%d", n-1) {
				http.Error(w, "spent refresh token", 400)
				return
			}
			refresh = fmt.Sprintf("refresh-%d", n)
		default:
			http.Error(w, "bad grant", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "same-access-token", "token_type": "Bearer", "refresh_token": refresh, "expires_in": 3600, "scope": "tools.read offline_access"})
	case "/mcp":
		if r.Header.Get("Authorization") != "Bearer same-access-token" && !f.noAuth.Load() {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf("Bearer resource_metadata=%q", f.http.URL+"/.well-known/oauth-protected-resource/mcp"))
			w.WriteHeader(401)
			io.WriteString(w, "{}")
			return
		}
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad MCP", 400)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method != "initialize" {
			http.Error(w, "unexpected MCP method", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": req.Params.ProtocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fixture", "version": "1"}}})
	default:
		http.NotFound(w, r)
	}
}

func (f *oauthFixture) fetch(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
	u, err := url.Parse(args.URL)
	if err != nil {
		return nil, err
	}
	query := u.Query()
	if query.Get("state") == "" || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" {
		f.t.Errorf("missing SDK state/PKCE: %v", query)
	}
	if query.Get("resource") != f.server.URL {
		f.t.Error("authorization URL missing resource indicator")
	}
	if strings.Contains(query.Get("scope"), "tools.write") {
		f.t.Error("explicit scopes broadened to every discovered scope")
	}
	f.mu.Lock()
	f.challenge = query.Get("code_challenge")
	f.mu.Unlock()
	redirect, err := url.Parse(f.server.OAuth.RedirectURL)
	if err != nil {
		return nil, err
	}
	redirect.RawQuery = url.Values{"code": {"test-code"}, "state": {query.Get("state")}, "iss": {f.http.URL}}.Encode()
	return ParseAuthorizationResponse(f.server.OAuth.RedirectURL, redirect.String())
}

func (f *oauthFixture) authorize(t *testing.T, store Store) sdkauth.OAuthHandler {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h, err := NewHandler(ctx, f.server, Options{Store: store, AuthorizationCodeFetcher: f.fetch})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL, nil)
	response, err := f.http.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Authorize(ctx, request, response); err != nil {
		t.Fatalf("authorize: %v (cause: %T)", err, errors.Unwrap(err))
	}
	// Later refresh must not retain the original connection context.
	cancel()
	return h
}

func TestSDKAuthorizationPKCEPersistenceRefreshAndLogout(t *testing.T) {
	f := newOAuthFixture(t)
	store := testStore(t)
	h := f.authorize(t, store)
	status, err := Inspect(context.Background(), f.server, Options{Store: store})
	if err != nil || !status.Authenticated || !status.CanRefresh || !status.Stored || status.Account != "work" {
		t.Fatalf("status: %+v %v", status, err)
	}
	key := CredentialKey(f.server)
	record, err := loadRecord(context.Background(), store, key)
	if err != nil {
		t.Fatal(err)
	}
	record.Token.Expiry = time.Now().Add(-time.Hour)
	record.Token.ExpiresIn = 3600
	if err := saveRecord(context.Background(), store, key, record); err != nil {
		t.Fatal(err)
	}
	otherStore, err := NewEncryptedFileStore(store.home, bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewHandler(context.Background(), f.server, Options{Store: otherStore})
	if err != nil {
		t.Fatal(err)
	}
	one, _ := h.TokenSource(context.Background())
	two, _ := restored.TokenSource(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			source := one
			if i%2 == 1 {
				source = two
			}
			token, err := source.Token()
			if err != nil || token == nil || token.AccessToken != "same-access-token" {
				t.Errorf("refresh: %v %v", token, err)
			}
		}(i)
	}
	wg.Wait()
	if got := f.refreshes.Load(); got != 1 {
		t.Fatalf("concurrent refreshes=%d, want 1", got)
	}
	record, err = loadRecord(context.Background(), store, key)
	if err != nil || record.Token.RefreshToken != "refresh-1" || !record.Token.Valid() {
		t.Fatalf("rotation not persisted: %+v %v", record, err)
	}
	if err := Logout(context.Background(), f.server, Options{Store: store}); err != nil {
		t.Fatal(err)
	}
	if _, err := two.Token(); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("old handler ignored logout: %v", err)
	}
	status, err = Inspect(context.Background(), f.server, Options{Store: store})
	if err != nil || status.Stored {
		t.Fatalf("logout status: %+v %v", status, err)
	}
}

func TestSDKLoginUsesRealMCPInitializationAndExplicitDCR(t *testing.T) {
	f := newOAuthFixture(t)
	f.server.OAuth.ClientID = ""
	f.server.OAuth.Issuer = ""
	f.server.OAuth.DynamicRegistration = true
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Login(ctx, f.server, Options{Store: store, AuthorizationCodeFetcher: f.fetch}); err != nil {
		t.Fatalf("login: %v (cause: %v)", err, errors.Unwrap(err))
	}
	if f.registrations.Load() != 1 {
		t.Fatalf("DCR registrations: %d", f.registrations.Load())
	}
	status, err := Inspect(ctx, f.server, Options{Store: store})
	if err != nil || !status.Stored {
		t.Fatalf("login persistence: %+v %v", status, err)
	}
}

func TestLoginDoesNotMistakeAnOldRecordForANewGrant(t *testing.T) {
	f := newOAuthFixture(t)
	store := testStore(t)
	f.authorize(t, store)
	f.noAuth.Store(true)
	err := Login(context.Background(), f.server, Options{Store: store, AuthorizationCodeFetcher: f.fetch})
	if err == nil || !strings.Contains(err.Error(), "without a new OAuth grant") {
		t.Fatalf("login accepted an old record without new authorization: %v", err)
	}
	status, inspectErr := Inspect(context.Background(), f.server, Options{Store: store})
	if inspectErr != nil || !status.Stored {
		t.Fatalf("unsuccessful new login removed existing credentials: %+v %v", status, inspectErr)
	}
}

type closeTracker struct {
	io.Reader
	closed bool
}

func (r *closeTracker) Close() error { r.closed = true; return nil }

func TestNonInteractiveHandlerGatesBeforeNetworkOrDCR(t *testing.T) {
	f := newOAuthFixture(t)
	f.server.OAuth.ClientID = ""
	f.server.OAuth.Issuer = ""
	f.server.OAuth.DynamicRegistration = true
	store := testStore(t)
	h, err := NewHandler(context.Background(), f.server, Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, f.server.URL, nil)
	body := &closeTracker{Reader: strings.NewReader("{}")}
	response := &http.Response{StatusCode: 401, Header: make(http.Header), Body: body}
	if err := h.Authorize(context.Background(), request, response); !errors.Is(err, ErrLoginRequired) {
		t.Fatal(err)
	}
	if !body.closed || f.requests.Load() != 0 {
		t.Fatalf("gate leaked work: closed=%v requests=%d", body.closed, f.requests.Load())
	}
	if _, err := NewHandler(context.Background(), config.Server{}, Options{Home: "does-not-exist"}); err != nil {
		t.Fatal(err)
	}
}

func TestAccountNamespacesDoNotShareCredentials(t *testing.T) {
	f := newOAuthFixture(t)
	store := testStore(t)
	f.authorize(t, store)
	other := f.server
	copy := *f.server.OAuth
	copy.Account = "personal"
	other.OAuth = &copy
	if CredentialKey(other) == CredentialKey(f.server) {
		t.Fatal("account missing from identity")
	}
	status, err := Inspect(context.Background(), other, Options{Store: store})
	if err != nil || status.Stored {
		t.Fatalf("cross-account reuse: %+v %v", status, err)
	}
	if err := Logout(context.Background(), other, Options{Store: store}); err != nil {
		t.Fatal(err)
	}
	status, err = Inspect(context.Background(), f.server, Options{Store: store})
	if err != nil || !status.Stored {
		t.Fatalf("logout removed other account: %+v %v", status, err)
	}
}

func TestCallbackValidationAndRedactedErrors(t *testing.T) {
	base := "http://127.0.0.1:48761/callback"
	for _, value := range []string{
		"http://attacker.test/callback?code=x&state=y",
		base + "?code=x&state=y&state=z", base + "?code=x",
		base + "?code=x&state=y#fragment", base + "?error=access_denied&error_description=secret",
	} {
		if _, err := ParseAuthorizationResponse(base, value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if got, err := ParseAuthorizationResponse(base, base+"?code=x&state=y&iss=https%3A%2F%2Fissuer.test"); err != nil || got.Iss != "https://issuer.test" {
		t.Fatalf("valid callback: %v %v", got, err)
	}
	secret := errors.New("token-response-secret")
	err := safeError("OAuth exchange", secret)
	if strings.Contains(err.Error(), "secret") || !errors.Is(err, secret) {
		t.Fatal("diagnostics expose secrets or lost cause")
	}
}
