package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

func TestInteractiveServerDefaultsPreserveCanonicalConfiguration(t *testing.T) {
	original := config.Server{Name: "remote", Source: "temporary-config", Type: "streamable-http", URL: "https://mcp.example.test/mcp"}
	effective := InteractiveServer(original)
	if original.OAuth != nil || effective.OAuth == nil || !effective.OAuth.DynamicRegistration {
		t.Fatalf("unexpected canonical/effective OAuth policy: %+v %+v", original.OAuth, effective.OAuth)
	}
	if err := effective.OAuth.Validate(); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(effective.OAuth.RedirectURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || port < 49152 || port > 65535 {
		t.Fatalf("unsafe implicit callback: %q", effective.OAuth.RedirectURL)
	}
	if !reflect.DeepEqual(effective, InteractiveServer(original)) || !reflect.DeepEqual(effective, InteractiveServer(effective)) {
		t.Fatal("runtime policy changed across repeated resolution")
	}
	explicit := original
	explicit.OAuth = &config.OAuthConfig{Account: "work", ClientID: "registered", Issuer: "https://issuer.example.test", RedirectURL: "http://127.0.0.1:48761/custom", Scopes: []string{"read"}}
	clone := InteractiveServer(explicit)
	if !reflect.DeepEqual(explicit, clone) {
		t.Fatal("explicit OAuth policy changed")
	}
	clone.OAuth.Scopes[0] = "write"
	clone.OAuth.RedirectURL = "http://127.0.0.1:48762/other"
	if explicit.OAuth.Scopes[0] != "read" || explicit.OAuth.RedirectURL != "http://127.0.0.1:48761/custom" {
		t.Fatal("effective policy aliases canonical OAuth data")
	}
	for _, test := range []struct {
		name   string
		mutate func(*config.Server)
	}{
		{"stdio", func(server *config.Server) { server.Type = "stdio" }},
		{"bearer", func(server *config.Server) { server.Headers = map[string]string{"aUtHoRiZaTiOn": "Bearer configured"} }},
		{"proxy credentials", func(server *config.Server) {
			server.Headers = map[string]string{"Proxy-Authorization": "Basic configured"}
		}},
		{"URL credentials", func(server *config.Server) { server.URL = "https://user:password@mcp.example.test/mcp" }},
		{"public intranet HTTP", func(server *config.Server) { server.URL = "http://mcp.internal/mcp" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := original
			test.mutate(&server)
			if InteractiveServer(server).OAuth != nil {
				t.Fatal("implicit OAuth replaced the existing authentication mechanism")
			}
			handler, err := NewInteractiveHandler(t.Context(), server, Options{Home: t.TempDir()})
			if err != nil || handler != nil {
				t.Fatalf("ineligible server got a handler: %T %v", handler, err)
			}
		})
	}
}

func TestInteractivePublicServerDoesNotResolveCredentialStore(t *testing.T) {
	t.Setenv("AGENTCFG_OAUTH_KEY", "irrelevant-invalid-key")
	home := t.TempDir()
	server := config.Server{Name: "public", Type: "streamable-http", URL: "https://mcp.example.test/mcp"}
	handler, err := NewInteractiveHandler(t.Context(), server, Options{Home: home})
	if err != nil || handler == nil {
		t.Fatalf("construct public handler: %T %v", handler, err)
	}
	for range 2 {
		if source, err := handler.TokenSource(t.Context()); err != nil || source != nil {
			t.Fatalf("public request touched the secure store: %T %v", source, err)
		}
	}
	state := handler.(*interactiveHandler)
	if state.AuthorizationPending() || state.AuthorizationAttempts() != 0 {
		t.Fatal("public token lookup was reported as OAuth activity")
	}
	request, response := interactiveChallenge(t, server.URL, "", http.StatusForbidden, "")
	if err := handler.Authorize(t.Context(), request, response); err != nil {
		t.Fatalf("ordinary forbidden response triggered consent: %v", err)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("public connection created credential state: %v %v", entries, err)
	}
}

func TestInteractiveImplicitLoginReconnectAndLogout(t *testing.T) {
	f := newOAuthFixture(t)
	f.server.OAuth = nil
	f.server = InteractiveServer(f.server)
	f.allowWrite = true
	store := testStore(t)
	var prompts atomic.Int32
	opts := Options{Store: store, AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		prompts.Add(1)
		return f.fetch(ctx, args)
	}}
	for session := range 2 {
		handler, err := NewInteractiveHandler(t.Context(), f.server, opts)
		if err != nil {
			t.Fatal(err)
		}
		if source, err := handler.TokenSource(t.Context()); err != nil || source != nil {
			t.Fatalf("new connection must await its first challenge: %T %v", source, err)
		}
		request, response := interactiveChallenge(t, f.server.URL, "", http.StatusUnauthorized, "")
		if err := handler.Authorize(t.Context(), request, response); err != nil {
			t.Fatal(err)
		}
		assertInteractiveToken(t, handler)
		if prompts.Load() != 1 || f.registrations.Load() != 1 {
			t.Fatalf("reconnect repeated consent/DCR: prompts=%d registrations=%d", prompts.Load(), f.registrations.Load())
		}
		if session == 1 {
			if err := Logout(t.Context(), f.server, Options{Store: store}); err != nil {
				t.Fatal(err)
			}
			if source, err := handler.TokenSource(t.Context()); err != nil || source != nil {
				t.Fatalf("logout did not allow fresh authorization: %T %v", source, err)
			}
			request, response = interactiveChallenge(t, f.server.URL, "", http.StatusUnauthorized, "")
			if err := handler.Authorize(t.Context(), request, response); err != nil {
				t.Fatal(err)
			}
			assertInteractiveToken(t, handler)
			if prompts.Load() != 2 || f.registrations.Load() != 2 {
				t.Fatalf("login after logout did not obtain new consent: prompts=%d registrations=%d", prompts.Load(), f.registrations.Load())
			}
		}
	}
}

func TestInteractiveRefreshAndStoreErrorsRemainDistinct(t *testing.T) {
	f := newOAuthFixture(t)
	store := testStore(t)
	f.authorize(t, store)
	var prompts atomic.Int32
	handler, err := NewInteractiveHandler(t.Context(), f.server, Options{Store: store, AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		prompts.Add(1)
		return f.fetch(ctx, args)
	}})
	if err != nil {
		t.Fatal(err)
	}
	request, response := interactiveChallenge(t, f.server.URL, "", http.StatusUnauthorized, "")
	if err := handler.Authorize(t.Context(), request, response); err != nil {
		t.Fatal(err)
	}
	expireRecord(t, store, CredentialKey(f.server))
	assertInteractiveToken(t, handler)
	if prompts.Load() != 0 || f.refreshes.Load() != 1 {
		t.Fatalf("refresh opened consent: prompts=%d refreshes=%d", prompts.Load(), f.refreshes.Load())
	}
	if err := store.Save(t.Context(), CredentialKey(f.server), []byte("invalid encrypted credential record")); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.TokenSource(t.Context()); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("store error was mistaken for missing consent: %v", err)
	}
}

func TestInteractiveRefreshActivityAndCancellation(t *testing.T) {
	f := newOAuthFixture(t)
	f.refreshStarted = make(chan struct{}, 1)
	f.refreshBlock = make(chan struct{})
	store := testStore(t)
	f.authorize(t, store)
	var prompts atomic.Int32
	h, err := NewInteractiveHandler(t.Context(), f.server, Options{Store: store, AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		prompts.Add(1)
		return f.fetch(ctx, args)
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler := h.(*interactiveHandler)
	request, response := interactiveChallenge(t, f.server.URL, "", http.StatusUnauthorized, "")
	if err := handler.Authorize(t.Context(), request, response); err != nil {
		t.Fatal(err)
	}
	expireRecord(t, store, CredentialKey(f.server))
	previous := handler.AuthorizationAttempts()
	guard, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	ctx, cancel := context.WithCancel(guard)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		source, err := handler.TokenSource(ctx)
		if source != nil {
			err = fmt.Errorf("cancelled refresh retained a token source: %w", err)
		}
		finished <- err
	}()
	select {
	case <-f.refreshStarted:
	case <-guard.Done():
		t.Fatal("refresh did not reach the blocking provider")
	}
	pending, attempts := handler.AuthorizationPending(), handler.AuthorizationAttempts()
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("refresh did not preserve request cancellation: %v", err)
		}
	case <-guard.Done():
		t.Fatal("blocked refresh ignored cancellation")
	}
	if !pending || attempts != previous+1 {
		t.Fatalf("discovery could not observe credential activity: pending=%t attempts=%d previous=%d", pending, attempts, previous)
	}
	if handler.AuthorizationPending() || handler.AuthorizationAttempts() != attempts {
		t.Fatal("cancelled credential activity remained pending or lost its attempt count")
	}
	if prompts.Load() != 0 || f.refreshes.Load() != 1 {
		t.Fatalf("cancelled refresh started consent or repeated the grant: prompts=%d refreshes=%d", prompts.Load(), f.refreshes.Load())
	}
}

func TestInteractiveRejectedBearerObtainsNewConsent(t *testing.T) {
	f := newOAuthFixture(t)
	store := testStore(t)
	f.authorize(t, store)
	before, err := loadRecord(t.Context(), store, CredentialKey(f.server))
	if err != nil {
		t.Fatal(err)
	}
	var prompts atomic.Int32
	handler, err := NewInteractiveHandler(t.Context(), f.server, Options{Store: store, AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		prompts.Add(1)
		return f.fetch(ctx, args)
	}})
	if err != nil {
		t.Fatal(err)
	}
	request, response := interactiveChallenge(t, f.server.URL, "Bearer same-access-token", http.StatusUnauthorized, "")
	if err := handler.Authorize(t.Context(), request, response); err != nil {
		t.Fatal(err)
	}
	after, err := loadRecord(t.Context(), store, CredentialKey(f.server))
	if err != nil || prompts.Load() != 1 || before.Generation == after.Generation {
		t.Fatalf("recently rejected bearer was reused without consent: prompts=%d err=%v", prompts.Load(), err)
	}
}

func TestInteractiveConsentErrorClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"missing", fmt.Errorf("wrapped: %w", ErrLoginRequired), true},
		{"invalid grant", safeError("refresh", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}), true},
		{"store", ErrStoreCorrupt, false},
		{"provider failure", &oauth2.RetrieveError{ErrorCode: "server_error"}, false},
		{"cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"cancelled missing grant", errors.Join(context.Canceled, ErrLoginRequired), false},
		{"expired invalid grant", errors.Join(context.DeadlineExceeded, &oauth2.RetrieveError{ErrorCode: "invalid_grant"}), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := needsInteractiveConsent(test.err); got != test.want {
				t.Fatalf("consent=%t want %t for %v", got, test.want, test.err)
			}
		})
	}
}

func interactiveChallenge(t *testing.T, endpoint, authorization string, status int, scopes string) (*http.Request, *http.Response) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", authorization)
	u, _ := url.Parse(endpoint)
	u.Path, u.RawQuery = "/.well-known/oauth-protected-resource/mcp", ""
	challenge := fmt.Sprintf("Bearer resource_metadata=%q", u.String())
	if scopes != "" {
		challenge += fmt.Sprintf(", scope=%q, error=\"insufficient_scope\"", scopes)
	}
	response := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ignored provider body"))}
	response.Header.Set("WWW-Authenticate", challenge)
	return request, response
}

func assertInteractiveToken(t *testing.T, handler sdkauth.OAuthHandler) {
	t.Helper()
	source, err := handler.TokenSource(t.Context())
	if err != nil || source == nil {
		t.Fatalf("missing token source: %T %v", source, err)
	}
	token, err := source.Token()
	if err != nil || token == nil || token.AccessToken != "same-access-token" {
		t.Fatalf("unexpected token: %v", err)
	}
}

// Done is first evaluated by run's final wait, after the caller has joined the
// flight. This gives the test a barrier without scheduling sleeps or polling.
type interactiveWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *interactiveWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestInteractiveAuthorizationPendingCountsSharedWaiters(t *testing.T) {
	f := newOAuthFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	owner, cancelOwner := context.WithCancel(ctx)
	defer cancelOwner()
	started, release := make(chan struct{}), make(chan struct{})
	var prompts atomic.Int32
	h, err := NewInteractiveHandler(ctx, f.server, Options{Store: testStore(t), AuthorizationCodeFetcher: func(flowContext context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		if prompts.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return f.fetch(flowContext, args)
		case <-flowContext.Done():
			return nil, flowContext.Err()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler := h.(*interactiveHandler)
	if handler.AuthorizationPending() || handler.AuthorizationAttempts() != 0 {
		t.Fatal("idle handler reported an authorization attempt")
	}
	ownerRequest, ownerResponse := interactiveChallenge(t, f.server.URL, "", http.StatusUnauthorized, "")
	waiterRequest, waiterResponse := interactiveChallenge(t, f.server.URL, "", http.StatusUnauthorized, "")
	ownerResult, waiterResult := make(chan error, 1), make(chan error, 1)
	go func() { ownerResult <- handler.Authorize(owner, ownerRequest, ownerResponse) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("consent did not start")
	}
	if !handler.AuthorizationPending() || handler.AuthorizationAttempts() != 1 {
		t.Fatal("browser consent was not reported as a pending attempt")
	}
	waiter := &interactiveWaitContext{Context: ctx, entered: make(chan struct{})}
	go func() { waiterResult <- handler.Authorize(waiter, waiterRequest, waiterResponse) }()
	select {
	case <-waiter.entered:
	case <-ctx.Done():
		t.Fatal("second caller did not join consent")
	}
	cancelOwner()
	if err := <-ownerResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("owner cancellation: %v", err)
	}
	if !handler.AuthorizationPending() || handler.AuthorizationAttempts() != 2 {
		t.Fatal("one cancellation hid another caller's pending authorization or attempt")
	}
	close(release)
	if err := <-waiterResult; err != nil {
		t.Fatal(err)
	}
	if handler.AuthorizationPending() || handler.AuthorizationAttempts() != 2 || prompts.Load() != 1 {
		t.Fatalf("completed consent remained pending, lost its attempt count, or was duplicated: pending=%t attempts=%d prompts=%d", handler.AuthorizationPending(), handler.AuthorizationAttempts(), prompts.Load())
	}
}

func TestInteractiveFlightKeepsRemainingWaiter(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	owner, cancelOwner := context.WithCancel(ctx)
	defer cancelOwner()
	var group interactiveFlightGroup
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	ownerResult, waiterResult := make(chan error, 1), make(chan error, 1)
	go func() {
		_, _, err := group.run(owner, "same-identity", func(flowContext context.Context) (bool, error) {
			calls.Add(1)
			started <- flowContext
			select {
			case <-release:
				return true, nil
			case <-flowContext.Done():
				return false, flowContext.Err()
			}
		})
		ownerResult <- err
	}()
	flowContext := <-started
	waiter := &interactiveWaitContext{Context: ctx, entered: make(chan struct{})}
	go func() {
		_, _, err := group.run(waiter, "same-identity", func(context.Context) (bool, error) {
			calls.Add(1)
			return false, errors.New("duplicated consent")
		})
		waiterResult <- err
	}()
	<-waiter.entered
	cancelOwner()
	if err := <-ownerResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("owner cancellation: %v", err)
	}
	if err := flowContext.Err(); err != nil {
		t.Fatalf("owner cancellation terminated another client's consent: %v", err)
	}
	close(release)
	if err := <-waiterResult; err != nil || calls.Load() != 1 {
		t.Fatalf("remaining waiter failed or duplicated consent: %v calls=%d", err, calls.Load())
	}
}

func TestInteractiveFlightWaitsForCancelledCallbackCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	owner, cancelOwner := context.WithCancel(ctx)
	defer cancelOwner()
	var group interactiveFlightGroup
	started, closing, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	first, second := make(chan error, 1), make(chan error, 1)
	var calls atomic.Int32
	go func() {
		_, _, err := group.run(owner, "same-identity", func(flowContext context.Context) (bool, error) {
			calls.Add(1)
			close(started)
			<-flowContext.Done()
			close(closing)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return false, flowContext.Err()
		})
		first <- err
	}()
	<-started
	cancelOwner()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-closing
	next := &interactiveWaitContext{Context: ctx, entered: make(chan struct{})}
	go func() {
		_, _, err := group.run(next, "same-identity", func(context.Context) (bool, error) {
			calls.Add(1)
			return true, nil
		})
		second <- err
	}()
	<-next.entered
	if calls.Load() != 1 {
		t.Fatal("new login raced a callback listener still being closed")
	}
	close(release)
	if err := <-second; err != nil || calls.Load() != 2 {
		t.Fatalf("fresh login failed after callback cleanup: %v calls=%d", err, calls.Load())
	}
}
