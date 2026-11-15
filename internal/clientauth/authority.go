// Package clientauth provides the client-facing OAuth authority for agentcfg's
// HTTP gateway. Its grants are independent of all upstream OAuth credentials.
package clientauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"agentcfg/internal/auth"
)

var (
	ErrInvalidToken = errors.New("client authorization is invalid or expired")
	ErrUnavailable  = errors.New("client authorization is unavailable")
)

const (
	scopeAccess   = "mcp:access"
	maxBody       = 16 << 10
	maxURL        = 1024
	maxClients    = 128
	maxFlows      = 64
	maxGrants     = 512
	maxCodes      = 128
	flowTTL       = 5 * time.Minute
	codeTTL       = time.Minute
	clientIdleTTL = 15 * time.Minute
	accessTTL     = 5 * time.Minute
	grantTTL      = 30 * 24 * time.Hour
)

// Resource identifies one configured gateway endpoint and its effective upstream
// identity. A configuration change must change Identity, invalidating old grants.
type Resource struct {
	URL      string
	Name     string
	Identity string
}

type Principal struct {
	ClientID string
	GrantID  string
	Resource string
}

type Options struct {
	Context         context.Context
	Issuer          string
	Store           auth.Store
	ResolveResource func(context.Context, string) (Resource, error)
	// Authorize completes and persists upstream authorization. It may call
	// present once to send the browser to the provider. The provider callback
	// must return the browser to completeURL; no upstream credential is returned.
	Authorize func(context.Context, Resource, Principal, func(string) error, string) error
}

type Authority struct {
	ctx       context.Context
	cancel    context.CancelFunc
	issuer    *url.URL
	store     auth.Store
	locker    auth.Locker
	resolve   func(context.Context, string) (Resource, error)
	authorize func(context.Context, Resource, Principal, func(string) error, string) error
	clientKey string
	grantKey  string
	lockKey   string
	now       func() time.Time

	mu     sync.Mutex
	flows  map[string]*flow
	closed bool
	wg     sync.WaitGroup
}

func New(opts Options) (*Authority, error) {
	issuer, err := url.Parse(opts.Issuer)
	if err != nil || issuer == nil || issuer.Host == "" || issuer.User != nil ||
		issuer.RawQuery != "" || issuer.Fragment != "" || issuer.RawFragment != "" ||
		(issuer.Path != "" && issuer.Path != "/") || issuer.RawPath != "" ||
		(issuer.Scheme != "https" && (issuer.Scheme != "http" || !loopback(issuer.Hostname()))) {
		return nil, errors.New("client authorization requires an HTTPS issuer or an HTTP loopback issuer without a path")
	}
	if opts.Store == nil || opts.ResolveResource == nil || opts.Authorize == nil {
		return nil, errors.New("client authorization requires a store, resource resolver, and authorizer")
	}
	locker, ok := opts.Store.(auth.Locker)
	if !ok {
		return nil, errors.New("client authorization store must support transaction locking")
	}
	issuer.Path = ""
	base := opts.Context
	if base == nil {
		base = context.Background()
	}
	if err := base.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(base)
	digest := sha256.Sum256([]byte(issuer.String()))
	namespace := "agentcfg/clientauth/v1/" + hex.EncodeToString(digest[:])
	return &Authority{
		ctx: ctx, cancel: cancel, issuer: issuer, store: opts.Store, locker: locker,
		resolve: opts.ResolveResource, authorize: opts.Authorize,
		clientKey: namespace + "/clients", grantKey: namespace + "/grants", lockKey: namespace + "/transaction",
		now: time.Now, flows: make(map[string]*flow),
	}, nil
}

// Close cancels outstanding consent transactions and joins their authorizers.
// Authorize implementations must honor their context, including during callback
// waiting. Existing persisted grants remain usable after an authority restart.
func (a *Authority) Close() {
	a.mu.Lock()
	a.closed = true
	a.cancel()
	for _, f := range a.flows {
		f.cancel()
	}
	a.mu.Unlock()
	a.wg.Wait()
}

// ValidateRequest is shared by OAuth and gateway handlers. It deliberately
// permits absent Origin for native clients, but never a foreign or null Origin.
func (a *Authority) ValidateRequest(w http.ResponseWriter, r *http.Request) bool {
	secureHeaders(w)
	if a.ctx.Err() != nil {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return false
	}
	if !strings.EqualFold(r.Host, a.issuer.Host) ||
		(r.URL.Host != "" && !strings.EqualFold(r.URL.Host, a.issuer.Host)) ||
		(r.URL.Scheme != "" && r.URL.Scheme != a.issuer.Scheme) {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	if origins := r.Header.Values("Origin"); len(origins) > 1 ||
		(len(origins) == 1 && origins[0] != a.issuer.String()) {
		oauthError(w, http.StatusForbidden, "access_denied")
		return false
	}
	return true
}

func (a *Authority) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !a.ValidateRequest(w, r) {
		return
	}
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server":
		if !method(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                         a.issuer.String(),
			"authorization_endpoint":                         a.endpoint("/oauth/authorize"),
			"token_endpoint":                                 a.endpoint("/oauth/token"),
			"registration_endpoint":                          a.endpoint("/oauth/register"),
			"response_types_supported":                       []string{"code"},
			"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
			"token_endpoint_auth_methods_supported":          []string{"none"},
			"code_challenge_methods_supported":               []string{"S256"},
			"scopes_supported":                               []string{scopeAccess},
			"authorization_response_iss_parameter_supported": true,
		})
	case "/oauth/register":
		a.register(w, r)
	case "/oauth/authorize":
		a.consent(w, r)
	case "/oauth/complete":
		a.complete(w, r)
	case "/oauth/token":
		a.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (a *Authority) Metadata(w http.ResponseWriter, r *http.Request, resource Resource) {
	if !a.ValidateRequest(w, r) || !method(w, r, http.MethodGet) {
		return
	}
	if !a.validResource(resource) {
		oauthError(w, http.StatusNotFound, "invalid_target")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource": resource.URL, "resource_name": resource.Name,
		"authorization_servers":    []string{a.issuer.String()},
		"scopes_supported":         []string{scopeAccess},
		"bearer_methods_supported": []string{"header"},
	})
}

func (a *Authority) Challenge(w http.ResponseWriter, r *http.Request, resource Resource) {
	a.challenge(w, r, resource, false)
}

func (a *Authority) ChallengeDenied(w http.ResponseWriter, r *http.Request, resource Resource) {
	a.challenge(w, r, resource, true)
}

func (a *Authority) challenge(w http.ResponseWriter, r *http.Request, resource Resource, denied bool) {
	if !a.ValidateRequest(w, r) {
		return
	}
	if !a.validResource(resource) {
		oauthError(w, http.StatusNotFound, "invalid_target")
		return
	}
	u, _ := url.Parse(resource.URL)
	metadata := a.endpoint("/.well-known/oauth-protected-resource") + u.EscapedPath()
	value := `Bearer resource_metadata="` + metadata + `", scope="` + scopeAccess + `"`
	status, code := http.StatusUnauthorized, "invalid_token"
	if denied {
		status, code = http.StatusForbidden, "insufficient_scope"
		value += `, error="insufficient_scope"`
	}
	w.Header().Set("WWW-Authenticate", value)
	oauthError(w, status, code)
}

func (a *Authority) validResource(resource Resource) bool {
	u, err := url.Parse(resource.URL)
	return err == nil && resource.Identity != "" && len(resource.Identity) <= 4096 &&
		len(resource.Name) <= 160 && len(resource.URL) <= maxURL &&
		u.Scheme == a.issuer.Scheme && u.Host == a.issuer.Host &&
		u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path != "" &&
		strings.HasPrefix(u.Path, "/") && u.RawPath == ""
}

func (a *Authority) resource(ctx context.Context, raw string) (Resource, error) {
	if raw == "" || len(raw) > maxURL {
		return Resource{}, ErrInvalidToken
	}
	resource, err := a.resolve(ctx, raw)
	if err != nil || resource.URL != raw || !a.validResource(resource) {
		return Resource{}, ErrInvalidToken
	}
	return resource, nil
}

func (a *Authority) endpoint(path string) string { return a.issuer.String() + path }

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func opaque(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func secureHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func oauthError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func method(w http.ResponseWriter, r *http.Request, allowed string) bool {
	if r.Method == allowed {
		return true
	}
	w.Header().Set("Allow", allowed)
	oauthError(w, http.StatusMethodNotAllowed, "invalid_request")
	return false
}

func form(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	if r.URL.RawQuery != "" || strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/x-www-form-urlencoded" {
		return nil, errors.New("invalid form")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	for _, values := range r.PostForm {
		if len(values) != 1 {
			return nil, errors.New("duplicate parameter")
		}
	}
	return r.PostForm, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, value any) error {
	if strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/json" || r.URL.RawQuery != "" {
		return errors.New("invalid JSON request")
	}
	reader := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := reader.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := reader.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
