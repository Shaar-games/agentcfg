package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/clientauth"
	"agentcfg/internal/config"
	"agentcfg/internal/wire"
)

const (
	proxySessionLimit   = 1024
	proxyChallengeLimit = 1024
	proxySessionIdle    = 30 * time.Minute
	proxyChallengeTTL   = 5 * time.Minute
)

// ProxyOptions keeps local client credentials separate from upstream OAuth.
// HTTPClient is optional and must not inject credentials or use a cookie jar.
type ProxyOptions struct {
	Home       string
	Store      auth.Store
	HTTPClient *http.Client
	Resolve    func(clientauth.Resource) (config.Server, error)
	// Challenge invalidates the affected local grant and emits a local OAuth
	// challenge. The final argument identifies an upstream insufficient_scope.
	Challenge func(http.ResponseWriter, *http.Request, clientauth.Resource, clientauth.Principal, bool)
}

type Proxy struct {
	opts       ProxyOptions
	client     *http.Client
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	sessions   map[string]*proxySession
	challenges map[string]upstreamChallenge
	authGates  map[string]*proxyAuthGate
	requests   chan struct{}
}

type proxySession struct {
	binding  string
	upstream string
	lastUsed time.Time
	active   int
}

type proxyAuthGate struct {
	ready chan struct{}
	users int
}

func NewProxy(opts ProxyOptions) *Proxy {
	ctx, cancel := context.WithCancel(context.Background())
	client := &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 45 * time.Second,
		IdleConnTimeout: 30 * time.Second, MaxIdleConns: 64, MaxIdleConnsPerHost: 32,
		MaxConnsPerHost: 64, MaxResponseHeaderBytes: 64 << 10, ForceAttemptHTTP2: true,
	}}
	if opts.HTTPClient != nil {
		*client = *opts.HTTPClient
	}
	// Redirects must never carry a configured secret or upstream token elsewhere.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	// A stream's lifetime belongs to the incoming request, including subscriptions.
	client.Timeout = 0
	return &Proxy{opts: opts, client: client, ctx: ctx, cancel: cancel,
		sessions: make(map[string]*proxySession), challenges: make(map[string]upstreamChallenge),
		authGates: make(map[string]*proxyAuthGate), requests: make(chan struct{}, 64)}
}

func (p *Proxy) Close() error {
	p.cancel()
	p.client.CloseIdleConnections()
	p.mu.Lock()
	clear(p.sessions)
	clear(p.challenges)
	p.mu.Unlock()
	return nil
}

func proxyBinding(resource clientauth.Resource, principal clientauth.Principal) string {
	value, _ := json.Marshal([]string{resource.Identity, principal.ClientID, principal.GrantID})
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func (p *Proxy) requestContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(p.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

// ServeMCP receives a request already authenticated by the local OAuth authority.
// It forwards HTTP bytes without reconstructing the MCP protocol or its tools.
func (p *Proxy) ServeMCP(w http.ResponseWriter, r *http.Request, resource clientauth.Resource, principal clientauth.Principal, server config.Server) {
	if principal.ClientID == "" || principal.GrantID == "" || resource.Identity == "" || principal.Resource != resource.URL {
		http.Error(w, "MCP authorization does not match this resource", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "MCP HTTP method is not supported", http.StatusMethodNotAllowed)
		return
	}
	select {
	case p.requests <- struct{}{}:
		defer func() { <-p.requests }()
	default:
		http.Error(w, "MCP gateway request capacity is full", http.StatusTooManyRequests)
		return
	}
	ctx, cancel := p.requestContext(r.Context())
	defer cancel()
	if ctx.Err() != nil {
		http.Error(w, "MCP gateway is closed", http.StatusServiceUnavailable)
		return
	}
	target, configured, err := proxyServer(server)
	if err != nil {
		http.Error(w, "Invalid upstream MCP configuration", http.StatusBadGateway)
		return
	}
	var body []byte
	if r.Body != nil {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, wire.MaxFrameSize))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "MCP request exceeds the size limit", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "MCP request body could not be read", http.StatusBadRequest)
			}
			return
		}
	}
	request, err := http.NewRequestWithContext(ctx, r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "MCP upstream request could not be created", http.StatusBadGateway)
		return
	}
	incomingHeaders := proxyRequestHeaders(r.Header)
	request.Header = incomingHeaders.Clone()
	for name, values := range configured {
		request.Header[name] = append([]string(nil), values...)
	}
	if target.User != nil && request.Header.Get("Authorization") == "" {
		password, _ := target.User.Password()
		request.SetBasicAuth(target.User.Username(), password)
	}
	request.URL.User = nil
	localSession, upstreamSession, release, ok := p.acquireSession(incomingHeaders, resource, principal)
	if !ok {
		http.Error(w, "MCP session is unknown or belongs to another authorization", http.StatusNotFound)
		return
	}
	defer release()
	request.Header.Del("Mcp-Session-Id")
	if upstreamSession != "" {
		request.Header.Set("Mcp-Session-Id", upstreamSession)
	}
	token, err := p.upstreamToken(ctx, server)
	if err != nil {
		if proxyNeedsConsent(err) {
			p.challenge(w, r, resource, principal, false)
		} else {
			http.Error(w, "MCP upstream credentials could not be loaded or refreshed", http.StatusBadGateway)
		}
		return
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := p.client.Do(request)
	if err != nil {
		// Transport failures do not establish whether an operation ran. Never replay.
		http.Error(w, "MCP upstream HTTP request failed; the operation is not retried", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 && response.StatusCode != http.StatusNotModified {
		http.Error(w, "MCP upstream redirect was rejected", http.StatusBadGateway)
		return
	}
	if needed, insufficient := proxyAuthorizationChallenge(response); needed && auth.InteractiveServer(server).OAuth != nil {
		if !p.rememberChallenge(resource, request, response) {
			http.Error(w, "MCP gateway authorization capacity is full", http.StatusServiceUnavailable)
			return
		}
		p.challenge(w, r, resource, principal, insufficient)
		return
	}
	if response.StatusCode == http.StatusUnauthorized {
		// Literal upstream credentials cannot be repaired by a local OAuth grant.
		http.Error(w, "MCP upstream rejected its configured credentials", http.StatusBadGateway)
		return
	}
	if response.StatusCode >= 400 {
		// HTTP diagnostics can contain credentials and private provider details.
		// Preserve only an actual MCP error belonging to this request; ordinary
		// upstream HTTP failures receive a fixed local diagnostic.
		data, readErr := io.ReadAll(io.LimitReader(response.Body, wire.MaxFrameSize+1))
		if readErr != nil || len(data) > wire.MaxFrameSize || response.Header.Get("Content-Encoding") != "" || !proxyCorrelatedError(data, body) {
			if retry := response.Header.Get("Retry-After"); retry != "" {
				w.Header().Set("Retry-After", retry)
			}
			http.Error(w, "MCP upstream rejected the HTTP request; the operation is not retried", response.StatusCode)
			return
		}
		response.Body = io.NopCloser(bytes.NewReader(data))
	}
	if values := response.Header.Values("Mcp-Session-Id"); len(values) != 0 {
		if len(values) != 1 || !proxyOpaqueHeader(values[0], 4096) {
			http.Error(w, "MCP upstream returned an invalid session identifier", http.StatusBadGateway)
			return
		}
		localSession, err = p.registerSession(localSession, values[0], resource, principal)
		if err != nil {
			http.Error(w, "MCP gateway session capacity is full", http.StatusServiceUnavailable)
			return
		}
	}
	if r.Method == http.MethodDelete && response.StatusCode >= 200 && response.StatusCode < 300 || response.StatusCode == http.StatusNotFound {
		p.removeSession(localSession)
	}
	for name, values := range proxyResponseHeaders(response.Header) {
		w.Header()[name] = values
	}
	if response.Header.Get("Mcp-Session-Id") != "" {
		w.Header().Set("Mcp-Session-Id", localSession)
	}
	w.WriteHeader(response.StatusCode)
	_ = http.NewResponseController(w).Flush()
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				return
			}
			_ = http.NewResponseController(w).Flush()
		}
		if readErr != nil {
			return
		}
	}
}

func proxyServer(server config.Server) (*url.URL, http.Header, error) {
	if server.Type != "http" && server.Type != "streamable-http" {
		return nil, nil, errors.New("HTTP upstream required")
	}
	u, err := url.Parse(server.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.Fragment != "" {
		return nil, nil, errors.New("invalid HTTP upstream")
	}
	headers := make(http.Header)
	for name, value := range server.Headers {
		canonical := http.CanonicalHeaderKey(name)
		if !proxyHeaderName(name) || strings.ContainsAny(value, "\r\n\x00") || headers[canonical] != nil || proxyHopHeader(canonical) || canonical == "Host" || canonical == "Content-Length" {
			return nil, nil, errors.New("invalid configured upstream header")
		}
		headers.Set(canonical, value)
	}
	return u, headers, nil
}

func proxyRequestHeaders(source http.Header) http.Header {
	header := make(http.Header)
	for name, values := range source {
		lower := strings.ToLower(name)
		if lower == "accept" || lower == "accept-encoding" || lower == "content-type" || lower == "content-encoding" || lower == "last-event-id" || strings.HasPrefix(lower, "mcp-") {
			header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
	proxyRemoveConnectionHeaders(header, source)
	return header
}

func proxyResponseHeaders(source http.Header) http.Header {
	header := make(http.Header)
	for name, values := range source {
		lower := strings.ToLower(name)
		switch lower {
		case "content-type", "content-length", "content-encoding", "cache-control", "etag", "last-modified", "retry-after", "vary":
			header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		default:
			if strings.HasPrefix(lower, "mcp-") && lower != "mcp-session-id" {
				header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
			}
		}
	}
	proxyRemoveConnectionHeaders(header, source)
	return header
}

func proxyRemoveConnectionHeaders(target, source http.Header) {
	for _, value := range source.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			target.Del(strings.TrimSpace(name))
		}
	}
	for name := range target {
		if proxyHopHeader(name) {
			delete(target, name)
		}
	}
}

func proxyHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

func proxyHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if c > 127 || !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	return true
}

func proxyOpaqueHeader(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, c := range value {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

func proxyCorrelatedError(response, request []byte) bool {
	var incoming, outgoing struct {
		Version string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *struct {
			Code    *int    `json:"code"`
			Message *string `json:"message"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(response, &incoming) != nil || json.Unmarshal(request, &outgoing) != nil || incoming.Version != "2.0" || incoming.Error == nil || incoming.Error.Code == nil || incoming.Error.Message == nil || incoming.Result != nil || len(outgoing.ID) == 0 || bytes.Equal(outgoing.ID, []byte("null")) {
		return false
	}
	var incomingString, outgoingString string
	if json.Unmarshal(incoming.ID, &incomingString) == nil && json.Unmarshal(outgoing.ID, &outgoingString) == nil {
		return incomingString == outgoingString
	}
	return bytes.Equal(bytes.TrimSpace(incoming.ID), bytes.TrimSpace(outgoing.ID))
}

func (p *Proxy) acquireSession(header http.Header, resource clientauth.Resource, principal clientauth.Principal) (string, string, func(), bool) {
	values := header.Values("Mcp-Session-Id")
	if len(values) == 0 {
		return "", "", func() {}, true
	}
	if len(values) != 1 || !proxyOpaqueHeader(values[0], 128) {
		return "", "", func() {}, false
	}
	id := values[0]
	p.mu.Lock()
	p.expireSessionsLocked(time.Now())
	session := p.sessions[id]
	if session == nil || session.binding != proxyBinding(resource, principal) {
		p.mu.Unlock()
		return "", "", func() {}, false
	}
	session.active++
	session.lastUsed = time.Now()
	upstream := session.upstream
	p.mu.Unlock()
	return id, upstream, func() { p.mu.Lock(); session.active--; session.lastUsed = time.Now(); p.mu.Unlock() }, true
}

func (p *Proxy) registerSession(id, upstream string, resource clientauth.Resource, principal clientauth.Principal) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireSessionsLocked(time.Now())
	if existing := p.sessions[id]; existing != nil {
		existing.upstream, existing.lastUsed = upstream, time.Now()
		return id, nil
	}
	if len(p.sessions) >= proxySessionLimit {
		return "", errors.New("session limit")
	}
	id = rand.Text()
	p.sessions[id] = &proxySession{binding: proxyBinding(resource, principal), upstream: upstream, lastUsed: time.Now()}
	return id, nil
}

func (p *Proxy) expireSessionsLocked(now time.Time) {
	for id, session := range p.sessions {
		if session.active == 0 && now.Sub(session.lastUsed) > proxySessionIdle {
			delete(p.sessions, id)
		}
	}
}

func (p *Proxy) removeSession(id string) { p.mu.Lock(); delete(p.sessions, id); p.mu.Unlock() }

func (p *Proxy) challenge(w http.ResponseWriter, r *http.Request, resource clientauth.Resource, principal clientauth.Principal, insufficient bool) {
	if p.opts.Challenge != nil {
		p.opts.Challenge(w, r, resource, principal, insufficient)
		return
	}
	http.Error(w, "MCP authorization is required", http.StatusUnauthorized)
}
