// Package gateway exposes configured HTTP MCP servers through a local OAuth
// resource server. Upstream credentials never become downstream credentials.
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"agentcfg/internal/auth"
	"agentcfg/internal/clientauth"
	"agentcfg/internal/config"
)

// Options supplies isolated dependencies for callers and tests. A nil Store
// selects the existing encrypted per-user store for the explicit source root.
type Options struct {
	Store      auth.Store
	HTTPClient *http.Client
}

// Handler owns the two independent OAuth legs and an HTTP-only MCP proxy. The
// stdio process broker remains on its private IPC transport.
type Handler struct {
	root      string
	baseURL   string
	routeKey  []byte
	authority *clientauth.Authority
	proxy     *Proxy
}

func New(ctx context.Context, root string, opts Options) (*Handler, error) {
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	if !cfg.Settings.Gateway.Enabled {
		return nil, errors.New("native OAuth gateway is disabled; enable gateway.enabled in agentcfg.json")
	}
	store := opts.Store
	if store == nil {
		store, err = auth.NewSystemStore(cfg.Root)
		if err != nil {
			return nil, fmt.Errorf("initialize encrypted gateway credentials: %w", err)
		}
	}
	routeKey, err := auth.GatewayKey(ctx, cfg.Root, store, true)
	if err != nil {
		return nil, fmt.Errorf("initialize protected gateway endpoints: %w", err)
	}
	h := &Handler{root: cfg.Root, baseURL: config.GatewayBaseURL(cfg.Settings.Gateway), routeKey: routeKey}
	h.proxy = NewProxy(ProxyOptions{
		Home: cfg.Root, Store: store, HTTPClient: opts.HTTPClient,
		Resolve: func(resource clientauth.Resource) (config.Server, error) {
			current, server, err := h.resolve(resource.URL)
			if err != nil || current.Identity != resource.Identity {
				return config.Server{}, errors.New("MCP configuration changed during authorization")
			}
			return server, nil
		},
		Challenge: func(w http.ResponseWriter, r *http.Request, resource clientauth.Resource, principal clientauth.Principal, insufficientScope bool) {
			// A local refresh alone cannot repair missing upstream consent. Revoke
			// this local grant so the client starts its native authorization flow.
			if err := h.authority.Revoke(r.Context(), principal); err != nil {
				gatewayError(w, http.StatusServiceUnavailable, "Authorization state is unavailable.")
				return
			}
			h.authority.Challenge(w, r, resource)
		},
	})
	h.authority, err = clientauth.New(clientauth.Options{
		Context: ctx, Issuer: h.baseURL, Store: store,
		ResolveResource: func(ctx context.Context, target string) (clientauth.Resource, error) {
			if err := ctx.Err(); err != nil {
				return clientauth.Resource{}, err
			}
			resource, _, err := h.resolve(target)
			return resource, err
		},
		Authorize: h.proxy.Authorize,
	})
	if err != nil {
		_ = h.proxy.Close()
		return nil, err
	}
	return h, nil
}

func (h *Handler) Close() error {
	h.authority.Close()
	return h.proxy.Close()
}

func (h *Handler) BaseURL() string { return h.baseURL }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authority.ValidateRequest(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/.well-known/oauth-authorization-server" || strings.HasPrefix(r.URL.Path, "/oauth/") {
		h.authority.ServeHTTP(w, r)
		return
	}
	metadata := strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource/mcp/")
	path := r.URL.EscapedPath()
	if metadata {
		path = strings.TrimPrefix(path, "/.well-known/oauth-protected-resource")
	} else if !strings.HasPrefix(path, "/mcp/") {
		http.NotFound(w, r)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		gatewayError(w, http.StatusBadRequest, "Unexpected MCP endpoint query.")
		return
	}
	resource, server, err := h.resolve(h.baseURL + path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if metadata {
		h.authority.Metadata(w, r, resource)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "GET, POST, DELETE")
		gatewayError(w, http.StatusMethodNotAllowed, "Unsupported MCP HTTP method.")
		return
	}
	token, ok := gatewayBearer(r)
	if !ok {
		h.authority.Challenge(w, r, resource)
		return
	}
	principal, err := h.authority.Verify(r.Context(), token, resource)
	if err != nil {
		if errors.Is(err, clientauth.ErrInvalidToken) {
			h.authority.Challenge(w, r, resource)
		} else {
			gatewayError(w, http.StatusServiceUnavailable, "Authorization state is unavailable.")
		}
		return
	}
	h.proxy.ServeMCP(w, r, resource, principal, server)
}

func gatewayBearer(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > 4096 {
		return "", false
	}
	scheme, token, separated := strings.Cut(values[0], " ")
	token = strings.TrimLeft(token, " ") // RFC 6750 permits one or more SP characters.
	if !separated || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func gatewayError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, message, status)
}

func (h *Handler) resolve(target string) (clientauth.Resource, config.Server, error) {
	u, err := url.Parse(target)
	if err != nil || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" {
		return clientauth.Resource{}, config.Server{}, errors.New("invalid MCP resource")
	}
	base, _ := url.Parse(h.baseURL)
	if u.Scheme != base.Scheme || u.Host != base.Host || u.RawPath != "" {
		return clientauth.Resource{}, config.Server{}, errors.New("unknown MCP resource")
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "mcp" || len(parts[2]) != 43 {
		return clientauth.Resource{}, config.Server{}, errors.New("invalid MCP resource path")
	}
	name, err := decodeRoutePart(parts[3], false)
	if err != nil {
		return clientauth.Resource{}, config.Server{}, err
	}
	harness, err := decodeRoutePart(parts[4], true)
	if err != nil {
		return clientauth.Resource{}, config.Server{}, err
	}
	cfg, err := config.Load(h.root)
	if err != nil {
		return clientauth.Resource{}, config.Server{}, err
	}
	if !cfg.Settings.Gateway.Enabled || config.GatewayBaseURL(cfg.Settings.Gateway) != h.baseURL {
		return clientauth.Resource{}, config.Server{}, errors.New("gateway configuration changed; restart the gateway")
	}
	server, found := cfg.Servers[name]
	if !found || (server.Type != "http" && server.Type != "streamable-http") || !config.Allows(server.Harnesses, harness) || !enabledHarness(cfg.Settings.Harnesses, harness) || !config.GatewayForHarness(cfg.Settings.Gateway, harness) {
		return clientauth.Resource{}, config.Server{}, errors.New("unknown or unavailable HTTP MCP server")
	}
	cfg.Settings.Gateway.RouteKey = h.routeKey
	if subtle.ConstantTimeCompare([]byte(config.GatewayResourceURL(cfg.Settings.Gateway, name, harness)), []byte(target)) != 1 {
		return clientauth.Resource{}, config.Server{}, errors.New("noncanonical MCP resource")
	}
	if targetsGateway(server.URL, base) {
		return clientauth.Resource{}, config.Server{}, errors.New("an upstream MCP cannot target the native OAuth gateway itself")
	}
	server.Name = name
	resource := clientauth.Resource{URL: target, Name: name, Identity: auth.CredentialKey(auth.InteractiveServer(server))}
	return resource, server, nil
}

// Reject loopback aliases at our listening port before any upstream probe or
// authorization can recursively discover this gateway's own OAuth authority.
func targetsGateway(target string, base *url.URL) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else if u.Scheme == "http" {
			port = "80"
		}
	}
	if port != base.Port() {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	address := net.ParseIP(host)
	return host == "localhost" || strings.HasSuffix(host, ".localhost") || (address != nil && address.IsLoopback())
}

func decodeRoutePart(value string, allowEmpty bool) (string, error) {
	if allowEmpty && value == "-" {
		return "", nil
	}
	if value == "" || len(value) > 4096 {
		return "", errors.New("invalid MCP route component")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) == 0 || !utf8.Valid(decoded) || strings.ContainsRune(string(decoded), 0) || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return "", errors.New("invalid MCP route component")
	}
	return string(decoded), nil
}

func enabledHarness(selected []string, name string) bool {
	if name == "" {
		return true
	}
	for _, value := range selected {
		if value == name {
			return true
		}
	}
	return false
}
