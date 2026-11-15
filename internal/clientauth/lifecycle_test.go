package clientauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/auth"
)

func TestClientAuthorizationCallbackWaitsForPersistedGrant(t *testing.T) {
	type upstreamFlow struct {
		complete string
		finish   chan struct{}
	}
	started := make(chan upstreamFlow, 1)
	f := newFixture(t, func(ctx context.Context, resource Resource, principal Principal, present func(string) error, complete string) error {
		finish := make(chan struct{})
		started <- upstreamFlow{complete: complete, finish: finish}
		if err := present("https://provider.example/authorize?state=separate-upstream-state"); err != nil {
			return err
		}
		select {
		case <-finish:
			return nil // This return models successful upstream persistence.
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	c := registerClient(t, f.a)
	flow := beginFlow(t, f, c, nil)
	w := request(f.a, http.MethodPost, "/oauth/authorize", flow.values.Encode(), flow.cookie)
	if !strings.HasPrefix(postNavigationTarget(t, w).String(), "https://provider.example/authorize?") {
		t.Fatalf("provider navigation: %d %v", w.Code, w.Header())
	}
	upstream := <-started
	complete, err := url.Parse(upstream.complete)
	if err != nil || complete.Host != "127.0.0.1:48991" || complete.Path != "/oauth/complete" ||
		complete.Query().Get("ticket") == "" || strings.Contains(upstream.complete, "client-state") {
		t.Fatalf("unbound completion URL: %s %v", upstream.complete, err)
	}
	withoutCookie := request(f.a, http.MethodGet, complete.RequestURI(), "", nil)
	if withoutCookie.Code != http.StatusBadRequest {
		t.Fatalf("completion without browser cookie: %d", withoutCookie.Code)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequestWithContext(cancelled, http.MethodGet, upstream.complete, nil)
	r.AddCookie(flow.cookie)
	pending := httptest.NewRecorder()
	f.a.ServeHTTP(pending, r)
	if pending.Header().Get("Location") != "" {
		t.Fatal("completion issued a code before upstream success")
	}
	close(upstream.finish)
	done := request(f.a, http.MethodGet, complete.RequestURI(), "", flow.cookie)
	target, _ := url.Parse(done.Header().Get("Location"))
	if done.Code != http.StatusSeeOther || target.Query().Get("code") == "" {
		t.Fatalf("completion: %d %s", done.Code, done.Body.String())
	}
	token := exchange(t, f.a, codeValues(flow, target.Query().Get("code")))
	if _, err := f.a.Verify(t.Context(), token.AccessToken, f.resource); err != nil {
		t.Fatal(err)
	}
	replayed := request(f.a, http.MethodGet, complete.RequestURI(), "", flow.cookie)
	if replayed.Code != http.StatusBadRequest || replayed.Header().Get("Location") != "" {
		t.Fatalf("completion ticket replay: %d", replayed.Code)
	}
}

func TestClientAuthorizationFailureDoesNotExposeProviderSecrets(t *testing.T) {
	const secret = "provider-secret-token-and-session"
	f := newFixture(t, func(context.Context, Resource, Principal, func(string) error, string) error {
		return errors.New("provider failed at https://provider.example/token?secret=" + secret)
	})
	c := registerClient(t, f.a)
	flow := beginFlow(t, f, c, nil)
	w := request(f.a, http.MethodPost, "/oauth/authorize", flow.values.Encode(), flow.cookie)
	target := postNavigationTarget(t, w)
	if target.Query().Get("error") != "access_denied" || target.Query().Get("code") != "" {
		t.Fatalf("provider failure: %d %s", w.Code, target)
	}
	if strings.Contains(w.Body.String()+w.Header().Get("Location"), secret) {
		t.Fatal("provider details leaked to the client")
	}
}

func TestClientAuthorizationCloseCancelsAndJoinsProvider(t *testing.T) {
	exited := make(chan struct{})
	f := newFixture(t, func(ctx context.Context, resource Resource, principal Principal, present func(string) error, complete string) error {
		defer close(exited)
		if err := present("https://provider.example/authorize"); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})
	c := registerClient(t, f.a)
	flow := beginFlow(t, f, c, nil)
	w := request(f.a, http.MethodPost, "/oauth/authorize", flow.values.Encode(), flow.cookie)
	_ = postNavigationTarget(t, w)
	f.a.Close()
	select {
	case <-exited:
	default:
		t.Fatal("Close returned before provider exited")
	}
	if _, err := f.a.Verify(t.Context(), "anything", f.resource); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("closed authority: %v", err)
	}
}

func TestClientAuthorizationConcurrentRefreshDetectsReplay(t *testing.T) {
	f := newFixture(t, nil)
	c, first := grantTokens(t, f)
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			<-start
			results <- request(f.a, http.MethodPost, "/oauth/token", refreshValues(c, f.resource, first.RefreshToken).Encode(), nil)
		}()
	}
	close(start)
	var winner tokenResponse
	successes := 0
	for range 2 {
		w := <-results
		if w.Code == http.StatusOK {
			successes++
			if err := json.Unmarshal(w.Body.Bytes(), &winner); err != nil {
				t.Fatal(err)
			}
		} else if w.Code != http.StatusBadRequest {
			t.Fatalf("concurrent refresh: %d", w.Code)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent refresh winners: %d", successes)
	}
	if _, err := f.a.Verify(t.Context(), winner.AccessToken, f.resource); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("replayed refresh family remained valid: %v", err)
	}
}

func TestClientAuthorizationRestartUsesEncryptedNamespacedState(t *testing.T) {
	f := newFixture(t, nil)
	home := t.TempDir()
	key := bytes.Repeat([]byte{0x69}, 32)
	store, err := auth.NewEncryptedFileStore(home, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(t.Context(), "upstream-fixture", []byte("upstream-credentials-stay-private")); err != nil {
		t.Fatal(err)
	}
	f.a.store, f.a.locker = store, store.(auth.Locker)
	c, first := grantTokens(t, f)
	firstPrincipal, err := f.a.Verify(t.Context(), first.AccessToken, f.resource)
	if err != nil {
		t.Fatal(err)
	}
	f.a.Close()
	otherStore, err := auth.NewEncryptedFileStore(home, key)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(Options{
		Context: t.Context(), Issuer: testIssuer, Store: otherStore,
		ResolveResource: f.a.resolve, Authorize: f.a.authorize,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	principal, err := restarted.Verify(t.Context(), first.AccessToken, f.resource)
	if err != nil || principal != firstPrincipal {
		t.Fatalf("persisted access grant: %+v %v", principal, err)
	}
	if registered, err := restarted.registered(t.Context(), c.ID); err != nil || registered.ID != c.ID {
		t.Fatalf("persisted registration: %+v %v", registered, err)
	}
	second := exchange(t, restarted, refreshValues(c, f.resource, first.RefreshToken))
	if _, err := restarted.Verify(t.Context(), second.AccessToken, f.resource); err != nil {
		t.Fatal(err)
	}
	original, err := otherStore.Load(t.Context(), "upstream-fixture")
	if err != nil || string(original) != "upstream-credentials-stay-private" {
		t.Fatalf("upstream credentials changed: %v", err)
	}
	err = filepath.WalkDir(home, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(path) != ".json" {
			return walkErr
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, value := range []string{c.ID, first.AccessToken, first.RefreshToken, f.resource.Identity, "upstream-credentials-stay-private"} {
			if bytes.Contains(raw, []byte(value)) {
				t.Errorf("plaintext authorization data in %s", filepath.Base(path))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestClientAuthorizationStoreFailureDoesNotBecomeAnonymous(t *testing.T) {
	f := newFixture(t, nil)
	c, token := grantTokens(t, f)
	f.store.mu.Lock()
	f.store.loadErr = errors.New("secret-store-path-and-token")
	f.store.mu.Unlock()
	if _, err := f.a.Verify(t.Context(), token.AccessToken, f.resource); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("store corruption downgraded to invalid token: %v", err)
	}
	w := request(f.a, http.MethodPost, "/oauth/token", refreshValues(c, f.resource, token.RefreshToken).Encode(), nil)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "secret-store") {
		t.Fatalf("store error response: %d %s", w.Code, w.Body.String())
	}
}

func TestClientAuthorizationFailedSaveDoesNotConsumeCode(t *testing.T) {
	f := newFixture(t, nil)
	c := registerClient(t, f.a)
	flow := beginFlow(t, f, c, nil)
	code := consentCode(t, f, flow)
	f.store.mu.Lock()
	f.store.saveErr = errors.New("encrypted persistence failed")
	f.store.mu.Unlock()
	w := request(f.a, http.MethodPost, "/oauth/token", codeValues(flow, code).Encode(), nil)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "encrypted persistence") {
		t.Fatalf("failed atomic exchange: %d %s", w.Code, w.Body.String())
	}
	f.store.mu.Lock()
	f.store.saveErr = nil
	f.store.mu.Unlock()
	_ = exchange(t, f.a, codeValues(flow, code))
}

func TestClientAuthorizationCodeAndConsentExpire(t *testing.T) {
	t.Run("code", func(t *testing.T) {
		f := newFixture(t, nil)
		c := registerClient(t, f.a)
		flow := beginFlow(t, f, c, nil)
		code := consentCode(t, f, flow)
		now := f.a.now()
		f.a.now = func() time.Time { return now.Add(codeTTL + time.Second) }
		w := request(f.a, http.MethodPost, "/oauth/token", codeValues(flow, code).Encode(), nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expired code: %d", w.Code)
		}
	})
	t.Run("consent", func(t *testing.T) {
		var calls atomic.Int32
		f := newFixture(t, func(context.Context, Resource, Principal, func(string) error, string) error {
			calls.Add(1)
			return nil
		})
		c := registerClient(t, f.a)
		flow := beginFlow(t, f, c, nil)
		now := f.a.now()
		f.a.now = func() time.Time { return now.Add(flowTTL + time.Second) }
		w := request(f.a, http.MethodPost, "/oauth/authorize", flow.values.Encode(), flow.cookie)
		if w.Code != http.StatusBadRequest || calls.Load() != 0 {
			t.Fatalf("expired consent: status=%d calls=%d", w.Code, calls.Load())
		}
	})
}

func TestClientAuthorizationRegistrationAndRequestBounds(t *testing.T) {
	f := newFixture(t, nil)
	for _, redirect := range []string{
		"http://evil.example/callback", "https://user:password@example.com/callback",
		"https://client.example/callback#fragment", "file:///tmp/callback",
		"http://127.0.0.1.evil.example/callback", "https://client.example/callback?code=injected",
	} {
		t.Run(redirect, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none"})
			r := httptest.NewRequest(http.MethodPost, testIssuer+"/oauth/register", bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			f.a.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("unsafe redirect accepted: %d", w.Code)
			}
		})
	}
	for _, scenario := range []string{"host", "origin", "oversize", "method"} {
		t.Run(scenario, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, testIssuer+"/oauth/register", strings.NewReader(`{"redirect_uris":["https://client.example/callback"]}`))
			r.Header.Set("Content-Type", "application/json")
			switch scenario {
			case "host":
				r.Host = "rebind.attacker.example"
			case "origin":
				r.Header.Set("Origin", "null")
			case "oversize":
				r = httptest.NewRequest(http.MethodPost, testIssuer+"/oauth/register", strings.NewReader(`{"client_name":"`+strings.Repeat("x", maxBody)+`","redirect_uris":["https://client.example/callback"]}`))
				r.Header.Set("Content-Type", "application/json")
			case "method":
				r.Method = http.MethodGet
			}
			w := httptest.NewRecorder()
			f.a.ServeHTTP(w, r)
			if w.Code < 400 {
				t.Fatalf("invalid request accepted: %d", w.Code)
			}
		})
	}
}

func TestClientAuthorizationNativeLoopbackPortExceptionIsNarrow(t *testing.T) {
	f := newFixture(t, nil)
	c := registerClient(t, f.a)
	for _, redirect := range []string{"http://127.0.0.1:54321/callback", "http://127.0.0.1:37651/other", "http://localhost:37651/callback", "https://127.0.0.1:37651/callback"} {
		t.Run(redirect, func(t *testing.T) {
			q := authorizationQuery(c, f.resource)
			q.Set("redirect_uri", redirect)
			w := request(f.a, http.MethodGet, "/oauth/authorize?"+q.Encode(), "", nil)
			want := http.StatusBadRequest
			if redirect == "http://127.0.0.1:54321/callback" {
				want = http.StatusOK
			}
			if w.Code != want {
				t.Fatalf("redirect status=%d want=%d", w.Code, want)
			}
		})
	}
}

func TestClientAuthorizationCapacityIsBounded(t *testing.T) {
	t.Run("clients", func(t *testing.T) {
		f := newFixture(t, nil)
		for range maxClients {
			_ = registerClient(t, f.a)
		}
		r := httptest.NewRequest(http.MethodPost, testIssuer+"/oauth/register", strings.NewReader(`{"redirect_uris":["https://client.example/callback"]}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.a.ServeHTTP(w, r)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("client capacity unbounded: %d", w.Code)
		}
	})
	t.Run("flows", func(t *testing.T) {
		f := newFixture(t, nil)
		c := registerClient(t, f.a)
		q := authorizationQuery(c, f.resource).Encode()
		for range maxFlows {
			_ = beginFlow(t, f, c, nil)
		}
		w := request(f.a, http.MethodGet, "/oauth/authorize?"+q, "", nil)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("flow capacity unbounded: %d", w.Code)
		}
	})
}
