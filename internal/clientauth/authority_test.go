package clientauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/auth"
)

type memoryStore struct {
	mu      sync.Mutex
	values  map[string][]byte
	locks   map[string]chan struct{}
	loadErr error
	saveErr error
}

func newMemoryStore() *memoryStore {
	return &memoryStore{values: make(map[string][]byte), locks: make(map[string]chan struct{})}
}

func (s *memoryStore) Load(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	value, ok := s.values[key]
	if !ok {
		return nil, auth.ErrNotFound
	}
	return bytes.Clone(value), nil
}

func (s *memoryStore) Save(ctx context.Context, key string, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.values[key] = bytes.Clone(value)
	return nil
}

func (s *memoryStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
	return nil
}

func (s *memoryStore) Lock(ctx context.Context, key string) (func() error, error) {
	s.mu.Lock()
	gate := s.locks[key]
	if gate == nil {
		gate = make(chan struct{}, 1)
		s.locks[key] = gate
	}
	s.mu.Unlock()
	select {
	case gate <- struct{}{}:
		return func() error { <-gate; return nil }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type fixture struct {
	a        *Authority
	store    *memoryStore
	resource Resource
}

type browserFlow struct {
	values url.Values
	cookie *http.Cookie
	client client
	query  url.Values
}

const testIssuer = "http://127.0.0.1:48991"
const testVerifier = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"

func newFixture(t *testing.T, authorize func(context.Context, Resource, Principal, func(string) error, string) error) *fixture {
	t.Helper()
	f := &fixture{store: newMemoryStore(), resource: Resource{
		URL: testIssuer + "/mcp/server-x", Name: "Server X", Identity: "configured-server-x-v1",
	}}
	if authorize == nil {
		authorize = func(context.Context, Resource, Principal, func(string) error, string) error { return nil }
	}
	a, err := New(Options{
		Context: t.Context(), Issuer: testIssuer, Store: f.store, Authorize: authorize,
		ResolveResource: func(ctx context.Context, raw string) (Resource, error) {
			if raw != f.resource.URL {
				return Resource{}, errors.New("unknown configured server")
			}
			return f.resource, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.a = a
	t.Cleanup(a.Close)
	return f
}

func request(a *Authority, method, target, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, testIssuer+target, strings.NewReader(body))
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if target == "/oauth/authorize" && method == http.MethodPost {
		r.Header.Set("Origin", testIssuer)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func registerClient(t *testing.T, a *Authority) client {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, testIssuer+"/oauth/register", strings.NewReader(`{
		"client_name":"Test Client","redirect_uris":["http://127.0.0.1:37651/callback"],
		"application_type":"native","grant_types":["authorization_code","refresh_token"],
		"response_types":["code"],"token_endpoint_auth_method":"none"
	}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("registration: %d %s", w.Code, w.Body.String())
	}
	var c client
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil || c.ID == "" {
		t.Fatalf("registration response: %v", err)
	}
	return c
}

func authorizationQuery(c client, resource Resource) url.Values {
	sum := sha256.Sum256([]byte(testVerifier))
	return url.Values{
		"response_type": {"code"}, "client_id": {c.ID}, "redirect_uri": {c.Redirects[0]},
		"resource": {resource.URL}, "scope": {scopeAccess}, "state": {"client-state"},
		"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
	}
}

var hiddenField = regexp.MustCompile(`name="(ticket|csrf)" value="([^"]+)"`)

func beginFlow(t *testing.T, f *fixture, c client, query url.Values) browserFlow {
	t.Helper()
	if query == nil {
		query = authorizationQuery(c, f.resource)
	}
	w := request(f.a, http.MethodGet, "/oauth/authorize?"+query.Encode(), "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("consent page: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatal("consent page cannot preserve the browser's same-origin form submission")
	}
	values := url.Values{"decision": {"allow"}}
	for _, found := range hiddenField.FindAllStringSubmatch(w.Body.String(), -1) {
		values.Set(found[1], found[2])
	}
	cookies := w.Result().Cookies()
	if values.Get("csrf") == "" || values.Get("ticket") == "" || len(cookies) != 1 || !cookies[0].HttpOnly ||
		cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("missing browser bindings: %v %v", values, cookies)
	}
	return browserFlow{values: values, cookie: cookies[0], client: c, query: query}
}

func consentCode(t *testing.T, f *fixture, flow browserFlow) string {
	t.Helper()
	w := request(f.a, http.MethodPost, "/oauth/authorize", flow.values.Encode(), flow.cookie)
	target := postNavigationTarget(t, w)
	if target.Query().Get("code") == "" ||
		target.Query().Get("state") != flow.query.Get("state") ||
		target.Query().Get("iss") != testIssuer {
		t.Fatalf("client authorization navigation: %s", target)
	}
	return target.Query().Get("code")
}

func codeValues(flow browserFlow, code string) url.Values {
	return url.Values{
		"grant_type": {"authorization_code"}, "client_id": {flow.client.ID},
		"redirect_uri": {flow.query.Get("redirect_uri")}, "resource": {flow.query.Get("resource")},
		"code": {code}, "code_verifier": {testVerifier},
	}
}

func exchange(t *testing.T, a *Authority, values url.Values) tokenResponse {
	t.Helper()
	w := request(a, http.MethodPost, "/oauth/token", values.Encode(), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("token exchange: %d %s", w.Code, w.Body.String())
	}
	var token tokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &token); err != nil || token.AccessToken == "" {
		t.Fatalf("token response: %v", err)
	}
	return token
}

func grantTokens(t *testing.T, f *fixture) (client, tokenResponse) {
	t.Helper()
	c := registerClient(t, f.a)
	flow := beginFlow(t, f, c, nil)
	code := consentCode(t, f, flow)
	return c, exchange(t, f.a, codeValues(flow, code))
}

func refreshValues(c client, resource Resource, token string) url.Values {
	return url.Values{
		"grant_type": {"refresh_token"}, "client_id": {c.ID},
		"resource": {resource.URL}, "refresh_token": {token},
	}
}

func TestClientAuthorizationRoundTripAndMetadata(t *testing.T) {
	var called atomic.Int32
	f := newFixture(t, func(ctx context.Context, resource Resource, principal Principal, present func(string) error, complete string) error {
		called.Add(1)
		if principal.ClientID == "" || principal.Resource != resource.URL || principal.GrantID != "" {
			return errors.New("invalid pre-grant principal")
		}
		return nil
	})
	metadata := request(f.a, http.MethodGet, "/.well-known/oauth-authorization-server", "", nil)
	if metadata.Code != http.StatusOK || strings.Contains(metadata.Body.String(), "client_id_metadata_document_supported") {
		t.Fatalf("unsupported metadata advertised: %d %s", metadata.Code, metadata.Body.String())
	}
	r := httptest.NewRequest(http.MethodPost, f.resource.URL, nil)
	w := httptest.NewRecorder()
	f.a.Challenge(w, r, f.resource)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), testIssuer+"/.well-known/oauth-protected-resource/mcp/server-x") {
		t.Fatalf("missing resource challenge: %d %v", w.Code, w.Header())
	}
	c, token := grantTokens(t, f)
	principal, err := f.a.Verify(t.Context(), token.AccessToken, f.resource)
	if err != nil || principal.ClientID != c.ID || principal.GrantID == "" || principal.Resource != f.resource.URL ||
		token.RefreshToken == "" || token.Scope != scopeAccess || called.Load() != 1 {
		t.Fatalf("grant result: principal=%+v err=%v authorizations=%d", principal, err, called.Load())
	}
}

func TestClientAuthorizationBindsCodeParametersAndConsumesOnce(t *testing.T) {
	for _, field := range []string{"client_id", "resource", "redirect_uri", "code_verifier"} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t, nil)
			c := registerClient(t, f.a)
			flow := beginFlow(t, f, c, nil)
			code := consentCode(t, f, flow)
			values := codeValues(flow, code)
			values.Set(field, "wrong-value")
			if field == "code_verifier" {
				values.Set(field, strings.Repeat("z", 43))
			}
			w := request(f.a, http.MethodPost, "/oauth/token", values.Encode(), nil)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_grant") {
				t.Fatalf("unbound %s accepted: %d %s", field, w.Code, w.Body.String())
			}
			_ = exchange(t, f.a, codeValues(flow, code))
			replay := request(f.a, http.MethodPost, "/oauth/token", codeValues(flow, code).Encode(), nil)
			if replay.Code != http.StatusBadRequest {
				t.Fatalf("code replay: %d", replay.Code)
			}
		})
	}
}

func TestClientAuthorizationConcurrentCodeExchangeHasOneWinner(t *testing.T) {
	f := newFixture(t, nil)
	c := registerClient(t, f.a)
	flow := beginFlow(t, f, c, nil)
	code := consentCode(t, f, flow)
	body := codeValues(flow, code).Encode()
	start := make(chan struct{})
	results := make(chan int, 8)
	for range 8 {
		go func() {
			<-start
			results <- request(f.a, http.MethodPost, "/oauth/token", body, nil).Code
		}()
	}
	close(start)
	winners := 0
	for range 8 {
		if status := <-results; status == http.StatusOK {
			winners++
		} else if status != http.StatusBadRequest {
			t.Fatalf("unexpected exchange status: %d", status)
		}
	}
	if winners != 1 {
		t.Fatalf("atomic code exchange winners: %d", winners)
	}
}

func TestClientAuthorizationConsentRejectsForgeryAndDenial(t *testing.T) {
	for _, scenario := range []string{"cookie", "csrf", "ticket", "origin", "deny", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			var called atomic.Int32
			f := newFixture(t, func(context.Context, Resource, Principal, func(string) error, string) error {
				called.Add(1)
				return nil
			})
			c := registerClient(t, f.a)
			flow := beginFlow(t, f, c, nil)
			switch scenario {
			case "cookie":
				flow.cookie.Value = "wrong"
			case "csrf", "ticket":
				flow.values.Set(scenario, "wrong")
			case "deny":
				flow.values.Set("decision", "deny")
			case "duplicate":
				flow.values.Add("decision", "deny")
			}
			r := httptest.NewRequest(http.MethodPost, testIssuer+"/oauth/authorize", strings.NewReader(flow.values.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", testIssuer)
			if scenario == "origin" {
				r.Header.Set("Origin", "https://attacker.example")
			}
			r.AddCookie(flow.cookie)
			w := httptest.NewRecorder()
			f.a.ServeHTTP(w, r)
			if scenario == "deny" {
				target := postNavigationTarget(t, w)
				if target.Query().Get("error") != "access_denied" || target.Query().Get("code") != "" {
					t.Fatalf("denial: %d %s", w.Code, target)
				}
			} else if w.Code < 400 {
				t.Fatalf("forged %s accepted: %d", scenario, w.Code)
			}
			if called.Load() != 0 {
				t.Fatalf("upstream authorization ran without consent: %d", called.Load())
			}
		})
	}
}

func TestClientAuthorizationRequiresPKCEAndRegisteredRedirect(t *testing.T) {
	for _, scenario := range []string{"plain", "missing", "short", "redirect", "resource", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t, nil)
			c := registerClient(t, f.a)
			q := authorizationQuery(c, f.resource)
			switch scenario {
			case "plain":
				q.Set("code_challenge_method", "plain")
			case "missing":
				q.Del("code_challenge")
			case "short":
				q.Set("code_challenge", "abc")
			case "redirect":
				q.Set("redirect_uri", "https://attacker.example/callback")
			case "resource":
				q.Set("resource", testIssuer+"/mcp/another-server")
			case "duplicate":
				q.Add("client_id", "attacker")
			}
			w := request(f.a, http.MethodGet, "/oauth/authorize?"+q.Encode(), "", nil)
			if w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" {
				t.Fatalf("invalid authorization accepted: %d %v", w.Code, w.Header())
			}
		})
	}
}

func TestClientAuthorizationRefreshRotationAndReplayRevokesFamily(t *testing.T) {
	f := newFixture(t, nil)
	c, first := grantTokens(t, f)
	second := exchange(t, f.a, refreshValues(c, f.resource, first.RefreshToken))
	if first.RefreshToken == second.RefreshToken || first.AccessToken == second.AccessToken {
		t.Fatal("refresh did not rotate credentials")
	}
	if _, err := f.a.Verify(t.Context(), first.AccessToken, f.resource); err != nil {
		t.Fatalf("unexpired access token invalidated by normal rotation: %v", err)
	}
	w := request(f.a, http.MethodPost, "/oauth/token", refreshValues(c, f.resource, first.RefreshToken).Encode(), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("refresh replay: %d", w.Code)
	}
	if _, err := f.a.Verify(t.Context(), second.AccessToken, f.resource); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("replay did not revoke access family: %v", err)
	}
	w = request(f.a, http.MethodPost, "/oauth/token", refreshValues(c, f.resource, second.RefreshToken).Encode(), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("replay did not revoke current refresh token: %d", w.Code)
	}
}

func TestClientAuthorizationForgedRefreshCannotRevokeGrant(t *testing.T) {
	f := newFixture(t, nil)
	c, token := grantTokens(t, f)
	payload, signature, err := parseToken(token.RefreshToken, "r1_", 40)
	if err != nil {
		t.Fatal(err)
	}
	signature[0] ^= 1
	forged := "r1_" + base64.RawURLEncoding.EncodeToString(append(payload, signature...))
	w := request(f.a, http.MethodPost, "/oauth/token", refreshValues(c, f.resource, forged).Encode(), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("forged refresh accepted: %d", w.Code)
	}
	if _, err := f.a.Verify(t.Context(), token.AccessToken, f.resource); err != nil {
		t.Fatalf("forgery revoked victim: %v", err)
	}
	_ = exchange(t, f.a, refreshValues(c, f.resource, token.RefreshToken))
}

func TestClientAuthorizationExpiryIdentityAndRevoke(t *testing.T) {
	f := newFixture(t, nil)
	_, token := grantTokens(t, f)
	principal, err := f.a.Verify(t.Context(), token.AccessToken, f.resource)
	if err != nil {
		t.Fatal(err)
	}
	other := f.resource
	other.Identity = "changed-server-configuration"
	if _, err := f.a.Verify(t.Context(), token.AccessToken, other); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("token survived identity change: %v", err)
	}
	wrong := principal
	wrong.ClientID = "another-client"
	if err := f.a.Revoke(t.Context(), wrong); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("foreign revoke accepted: %v", err)
	}
	now := f.a.now()
	f.a.now = func() time.Time { return now.Add(accessTTL + time.Second) }
	if _, err := f.a.Verify(t.Context(), token.AccessToken, f.resource); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token accepted: %v", err)
	}
	f.a.now = time.Now
	if err := f.a.Revoke(t.Context(), principal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.Verify(t.Context(), token.AccessToken, f.resource); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token accepted: %v", err)
	}
}
