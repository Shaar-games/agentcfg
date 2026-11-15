package auth

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"agentcfg/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// InteractiveServer returns the effective OAuth policy for an interactive
// connection. The defaults are runtime-only: the canonical server is unchanged.
// A stable loopback redirect also keeps the credential identity stable across
// clients, broker restarts, and explicit auth status/logout commands.
func InteractiveServer(server config.Server) config.Server {
	if server.Type == "http" {
		server.Type = "streamable-http"
	}
	if server.OAuth != nil {
		policy := *server.OAuth
		policy.Scopes = append([]string(nil), policy.Scopes...)
		server.OAuth = &policy
		return server
	}
	if server.Type != "streamable-http" && server.Type != "http" {
		return server
	}
	u, err := url.Parse(server.URL)
	if err != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return server
	}
	if u.Scheme == "http" {
		address, _ := netip.ParseAddr(u.Hostname())
		if !strings.EqualFold(u.Hostname(), "localhost") && !address.IsLoopback() {
			// Ordinary HTTP remains available for public/intranet MCP servers.
			// Implicit OAuth must not weaken its HTTPS/loopback-only policy.
			return server
		}
	}
	for name := range server.Headers {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") {
			return server
		}
	}
	server.Type = "streamable-http"
	identity := sha256.Sum256([]byte(CredentialKey(server)))
	port := 49152 + int(binary.BigEndian.Uint16(identity[:2]))%16384
	server.OAuth = &config.OAuthConfig{
		RedirectURL:         fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", port),
		DynamicRegistration: true,
	}
	return server
}

// NewInteractiveHandler supports browser consent supplied by the caller, without
// opening a browser or listener itself. Explicit OAuth uses stored credentials
// from the first request. URL-only servers defer store access until a challenge,
// so public HTTP servers work without a configured secure store. Pass the original
// server configuration; this constructor supplies InteractiveServer defaults.
func NewInteractiveHandler(ctx context.Context, server config.Server, opts Options) (sdkauth.OAuthHandler, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	explicit := server.OAuth != nil
	server = InteractiveServer(server)
	if server.OAuth == nil {
		return nil, nil
	}
	if err := validateServer(server); err != nil {
		return nil, err
	}
	return &interactiveHandler{server: server, options: opts, key: CredentialKey(server), explicit: explicit, initialize: make(chan struct{}, 1)}, nil
}

type interactiveHandler struct {
	server     config.Server
	options    Options
	key        string
	explicit   bool
	initialize chan struct{}
	pending    atomic.Int32
	attempts   atomic.Uint64
	mu         sync.Mutex
	state      *interactiveState
}

type interactiveState struct {
	store  Store
	source *tokenSource
	flowID string
}

var nextInteractiveStore atomic.Uint64

func (h *interactiveHandler) credentials(ctx context.Context) (*interactiveState, error) {
	h.mu.Lock()
	state := h.state
	h.mu.Unlock()
	if state != nil {
		return state, nil
	}
	select {
	case h.initialize <- struct{}{}:
		defer func() { <-h.initialize }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	h.mu.Lock()
	state = h.state
	h.mu.Unlock()
	if state != nil {
		return state, nil
	}
	store, locker, err := resolveStore(h.options)
	if err != nil {
		return nil, err
	}
	client, err := newHTTPClient(h.server, h.options.HTTPClient)
	if err != nil {
		return nil, err
	}
	base := context.WithValue(context.Background(), oauth2.HTTPClient, client)
	state = &interactiveState{store: store,
		source: managedTokenSource(base, store, locker, h.key, &scopeState{}, h.server.OAuth),
		flowID: interactiveStoreIdentity(store) + ":" + h.key,
	}
	h.mu.Lock()
	h.state = state
	h.mu.Unlock()
	return state, nil
}

// Independent file-store instances for the same directory share consent. An
// injected store is scoped to its instance, so one test/application cannot join
// another store's login solely because their resource configurations match.
func interactiveStoreIdentity(store Store) string {
	if file, ok := store.(*fileStore); ok {
		return "file:" + file.dir
	}
	value := reflect.ValueOf(store)
	if value.Kind() == reflect.Pointer {
		return fmt.Sprintf("custom:%T:%x", store, value.Pointer())
	}
	return fmt.Sprintf("custom-instance:%d", nextInteractiveStore.Add(1))
}

func (h *interactiveHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	state := h.state
	h.mu.Unlock()
	if state == nil && !h.explicit {
		return nil, nil
	}
	h.pending.Add(1)
	h.attempts.Add(1)
	defer h.pending.Add(-1)
	if state == nil {
		var err error
		state, err = h.credentials(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
	}
	token, err := state.source.token(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if needsInteractiveConsent(err) {
		// A missing/deleted/expired grant must reach the resource without a
		// bearer token so the SDK can obtain a fresh authorization challenge.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return oauth2.StaticTokenSource(token), nil
}

func needsInteractiveConsent(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrLoginRequired) {
		return true
	}
	var retrieve *oauth2.RetrieveError
	return errors.As(err, &retrieve) && retrieve.ErrorCode == "invalid_grant"
}

// AuthorizationPending reports whether a caller is waiting for stored credentials,
// refresh, discovery, registration, shared consent, or token exchange. It does not
// expose URLs or credentials and does not extend the caller's request context.
func (h *interactiveHandler) AuthorizationPending() bool { return h.pending.Load() != 0 }

// AuthorizationAttempts counts credential and authorization operations so a short
// discovery probe can notice OAuth activity even if it finished before the timer.
// URL-only requests that have never received a challenge do not increment it.
func (h *interactiveHandler) AuthorizationAttempts() uint64 { return h.attempts.Load() }

func (h *interactiveHandler) Authorize(ctx context.Context, request *http.Request, response *http.Response) error {
	if response == nil || response.Body == nil || request == nil || request.URL == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return fmt.Errorf("OAuth authorization requires an HTTP challenge")
	}
	// The SDK only needs the challenge headers, not the provider response body.
	// Release the failed request before waiting for another client's consent;
	// that request may be cancelled while the shared authorization continues.
	if err := ctx.Err(); err != nil {
		_ = response.Body.Close()
		return err
	}
	h.pending.Add(1)
	h.attempts.Add(1)
	defer h.pending.Add(-1)
	_ = response.Body.Close()
	challenges, err := oauthex.ParseWWWAuthenticate(response.Header.Values("WWW-Authenticate"))
	if err != nil {
		return safeError("parse OAuth challenge", err)
	}
	var required []string
	insufficientScope := false
	for _, challenge := range challenges {
		if challenge.Scheme == "bearer" {
			if len(required) == 0 {
				required = strings.Fields(challenge.Params["scope"])
			}
			insufficientScope = insufficientScope || challenge.Params["error"] == "insufficient_scope"
		}
	}
	if response.StatusCode == http.StatusForbidden && !insufficientScope {
		// Match the SDK: ordinary access denial is not a request for consent.
		return nil
	}
	state, err := h.credentials(ctx)
	if err != nil {
		return err
	}
	u := *request.URL
	headers, status := response.Header.Clone(), response.StatusCode
	method, requestHeaders := request.Method, request.Header.Clone()
	authorization := requestHeaders.Get("Authorization")
	for attempt := 0; attempt < 2; attempt++ {
		owned, consented, err := interactiveLogins.run(ctx, state.flowID, func(flowContext context.Context) (bool, error) {
			token, tokenErr := state.source.token(flowContext)
			if tokenErr == nil && authorization != "Bearer "+token.AccessToken && includesScopes(state.source.scopes.get(), required) {
				// Another handler/process may already have completed consent, or
				// this is the first challenge after a broker restart.
				return false, nil
			}
			if tokenErr != nil && !needsInteractiveConsent(tokenErr) {
				return false, tokenErr
			}
			opts := h.options
			opts.Store = state.store
			oauth, err := newHandler(flowContext, h.server, opts, true)
			if err != nil {
				return false, err
			}
			req := (&http.Request{Method: method, URL: &u, Header: requestHeaders.Clone()}).WithContext(flowContext)
			resp := &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(""))}
			err = oauth.Authorize(flowContext, req, resp)
			return oauth.(*handler).persisted.Load(), err
		})
		if err != nil || owned {
			return err
		}
		// Different simultaneous challenges can require different scopes. A
		// waiter may reuse a successful flow only if it covers this challenge;
		// otherwise allow one step-up flow with its own requested permissions.
		token, err := state.source.token(ctx)
		if err != nil {
			return err
		}
		if token != nil && includesScopes(state.source.scopes.get(), required) && (consented || authorization != "Bearer "+token.AccessToken) {
			return nil
		}
	}
	return fmt.Errorf("OAuth authorization did not grant the required scopes")
}

func includesScopes(granted, required []string) bool {
	for _, scope := range required {
		if !slices.Contains(granted, scope) {
			return false
		}
	}
	return true
}
