package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/wire"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

const (
	httpRequestLimit      = 8
	httpControlLimit      = 4
	httpSubscriptionLimit = 8
	httpRequestTimeout    = 2 * time.Minute
	httpStreamLifetime    = 5 * time.Minute
	httpCloseTimeout      = 2 * time.Second
)

var ErrBusy = errors.New("MCP HTTP transport concurrency limit reached")

type httpState uint8

const (
	httpNew httpState = iota
	httpInitializing
	httpNeedsInitialized
	httpSendingInitialized
	httpReady
	httpModern // 2026-07-28: no session, handshake or unsolicited GET stream
)

type httpPending struct {
	cancel     context.CancelFunc
	initialize bool
	discover   bool
	done       chan struct{}
}

type routedMessage struct {
	message wire.Message
	parent  json.RawMessage
}

type httpTransport struct {
	ctx               context.Context
	cancel            context.CancelFunc
	endpoint          string
	headers           http.Header
	client            *http.Client
	network           *http.Transport
	incoming          chan routedMessage
	requests          chan struct{}
	controls          chan struct{}
	subscriptions     chan struct{}
	ready             chan struct{}
	mu                sync.Mutex
	state             httpState
	modernEstablished bool
	session           string
	protocol          string
	offeredProtocol   string
	toolHeaders       map[string][]toolParameterHeader
	toolCatalogs      map[string]*toolHeaderCatalog
	closed            bool
	failure           error
	pending           map[string]*httpPending
	wg                sync.WaitGroup
	closeOnce         sync.Once
	oauth             sdkauth.OAuthHandler
}

func startHTTP(parent context.Context, s config.Server) (Transport, error) {
	return startHTTPWithOAuth(parent, s, nil)
}

func startHTTPWithOAuth(parent context.Context, s config.Server, opts *auth.Options) (Transport, error) {
	u, err := url.Parse(s.URL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.Host == "" || u.Hostname() == "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("MCP HTTP URL must be an absolute http(s) endpoint without a fragment")
	}
	headers := make(http.Header)
	for name, value := range s.Headers {
		if !validHeaderName(name) || !validHeaderValue(value) {
			return nil, errors.New("invalid configured MCP HTTP header")
		}
		name = http.CanonicalHeaderKey(name)
		if _, exists := headers[name]; exists {
			return nil, errors.New("duplicate case-insensitive MCP HTTP header")
		}
		switch name {
		case "Host", "Content-Length", "Transfer-Encoding", "Connection", "Upgrade":
			return nil, errors.New("configured MCP HTTP headers cannot override connection framing or host")
		}
		headers.Set(name, value)
	}
	ctx, cancel := context.WithCancel(parent)
	network := &http.Transport{
		Proxy:                  http.ProxyFromEnvironment,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:      true,
		MaxConnsPerHost:        httpRequestLimit + httpControlLimit + httpSubscriptionLimit + 1,
		MaxIdleConns:           httpRequestLimit + httpControlLimit + httpSubscriptionLimit + 1,
		MaxIdleConnsPerHost:    httpRequestLimit + httpControlLimit + httpSubscriptionLimit + 1,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  httpRequestTimeout,
		MaxResponseHeaderBytes: 64 << 10,
	}
	t := &httpTransport{ctx: ctx, cancel: cancel, endpoint: u.String(), headers: headers,
		network: network, incoming: make(chan routedMessage, 8), requests: make(chan struct{}, httpRequestLimit),
		controls: make(chan struct{}, httpControlLimit), subscriptions: make(chan struct{}, httpSubscriptionLimit),
		ready: make(chan struct{}), pending: make(map[string]*httpPending)}
	t.client = &http.Client{Transport: network, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	if opts == nil {
		t.oauth, err = auth.NewHandler(ctx, s, auth.Options{})
	} else {
		t.oauth, err = auth.NewInteractiveHandler(ctx, s, *opts)
	}
	if err != nil {
		cancel()
		network.CloseIdleConnections()
		return nil, err
	}
	go func() { <-ctx.Done(); _ = t.Close() }()
	return t, nil
}

func (t *httpTransport) PID() int { return 0 }

// AuthorizationPending lets the broker distinguish a quick capability probe
// from a discovery request waiting for interactive user authorization.
func (t *httpTransport) AuthorizationPending() bool {
	state, ok := t.oauth.(interface{ AuthorizationPending() bool })
	return ok && state.AuthorizationPending()
}

func (t *httpTransport) AuthorizationAttempts() uint64 {
	state, ok := t.oauth.(interface{ AuthorizationAttempts() uint64 })
	if !ok {
		return 0
	}
	return state.AuthorizationAttempts()
}

func (t *httpTransport) Read() (wire.Message, error) {
	message, _, err := t.ReadRouted()
	return message, err
}

func (t *httpTransport) ReadRouted() (wire.Message, json.RawMessage, error) {
	select {
	case message := <-t.incoming:
		return message.message, message.parent, nil
	default:
	}
	select {
	case message := <-t.incoming:
		return message.message, message.parent, nil
	case <-t.ctx.Done():
		select {
		case message := <-t.incoming:
			return message.message, message.parent, nil
		default:
		}
		t.mu.Lock()
		err := t.failure
		t.mu.Unlock()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, io.EOF
	}
}

func isRequest(message wire.Message) bool { return message.Method() != "" && message.HasID() }

func (t *httpTransport) Write(message wire.Message) error {
	method := message.Method()
	request := isRequest(message)
	initialize := method == "initialize" && request
	initialized := method == "notifications/initialized" && !message.HasID()
	discover := method == "server/discover" && request
	modern := discover || wire.IsModernRequest(message)
	var offered struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if initialize && (json.Unmarshal(message["params"], &offered) != nil || !visibleASCII(offered.ProtocolVersion, 128)) {
		return errors.New("MCP HTTP initialize requires a valid protocolVersion")
	}
	control := !request
	lane := t.requests
	if control {
		lane = t.controls
	} else if modern && method == "subscriptions/listen" {
		lane = t.subscriptions
	}

	t.mu.Lock()
	if t.closed || t.ctx.Err() != nil {
		t.mu.Unlock()
		return ErrClosed
	}
	if method == "notifications/cancelled" && !message.HasID() {
		pending := t.cancelNotificationLocked(message)
		if pending == nil {
			// Ignore unknown or completed requests and initialize, which MCP
			// clients must not cancel. A late notification can arrive after a
			// failed discovery has already reset the transport to httpNew.
			t.mu.Unlock()
			return nil
		}
		// Stateless cancellation closes the POST without a notification POST.
		// A discovery can already be resetting its protocol state, so identify
		// it by the pending request as well as the transport's current mode.
		if t.state == httpModern || pending.discover {
			t.mu.Unlock()
			if pending.discover {
				// Drain the probe before a legacy initialize can reuse its state.
				<-pending.done
			}
			return nil
		}
	}
	if modern && t.state != httpNew && t.state != httpModern {
		t.mu.Unlock()
		return errors.New("MCP 2026 request cannot use a legacy HTTP session; configure sharing=isolated")
	}
	if t.state == httpModern && !modern {
		t.mu.Unlock()
		return errors.New("legacy MCP request cannot use a stateless 2026 HTTP transport")
	}
	if initialize && t.state != httpNew {
		t.mu.Unlock()
		return errors.New("MCP HTTP session is already initializing or initialized")
	}
	if initialized && t.state != httpNeedsInitialized {
		t.mu.Unlock()
		return errors.New("MCP HTTP initialize result must precede notifications/initialized")
	}
	if discover && !wire.IsModernRequest(message) {
		t.mu.Unlock()
		return errors.New("MCP 2026 server/discover requires per-request protocol version and client capabilities")
	}
	// Callback responses must remain writable even during initialize, and must
	// not compete for slots held by SSE requests waiting for those callbacks.
	if !modern && !initialize && !initialized && method != "" && t.state < httpSendingInitialized {
		t.mu.Unlock()
		return errors.New("MCP HTTP initialize/initialized handshake is incomplete")
	}
	key := requestIDKey(message.ID())
	if request && t.pending[key] != nil {
		t.mu.Unlock()
		return errors.New("MCP HTTP request ID is already pending")
	}
	select {
	case lane <- struct{}{}:
	default:
		t.mu.Unlock()
		return ErrBusy
	}
	data, err := json.Marshal(message)
	if err != nil || len(data) > wire.MaxFrameSize {
		<-lane
		t.mu.Unlock()
		return errors.New("invalid MCP HTTP message or frame exceeds 32 MiB")
	}
	if _, err := decodeSDKMessage(message); err != nil {
		<-lane
		t.mu.Unlock()
		return err
	}
	if modern && t.state == httpNew {
		t.state = httpModern
	}
	if initialize {
		t.state = httpInitializing
		t.offeredProtocol = offered.ProtocolVersion
	}
	if initialized {
		t.state = httpSendingInitialized
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if modern && method == "subscriptions/listen" {
		ctx, cancel = context.WithCancel(t.ctx)
	} else {
		ctx, cancel = context.WithTimeout(t.ctx, httpRequestTimeout)
	}
	pending := &httpPending{cancel: cancel, initialize: initialize, discover: discover, done: make(chan struct{})}
	if request {
		t.pending[key] = pending
	}
	waitReady := !initialize && !initialized && method != "" && t.state != httpReady && t.state != httpModern
	t.wg.Add(1)
	t.mu.Unlock()
	id := append(json.RawMessage(nil), message.ID()...)
	go func() {
		defer t.wg.Done()
		defer cancel()
		defer func() { <-lane }()
		defer func() {
			if request {
				t.mu.Lock()
				if t.pending[key] == pending {
					delete(t.pending, key)
				}
				t.mu.Unlock()
			}
			close(pending.done)
		}()
		if waitReady {
			select {
			case <-t.ready:
			case <-ctx.Done():
				t.postFailure(id, request, initialize, discover, requestError(ctx, ctx.Err()), false)
				return
			}
		}
		err := t.post(ctx, data, id, request, initialize, initialized, discover)
		if err != nil {
			t.postFailure(id, request, initialize, discover, err, errors.Is(err, errSessionExpired))
		}
	}()
	return nil
}

func (t *httpTransport) cancelNotificationLocked(message wire.Message) *httpPending {
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(message["params"], &params) == nil {
		if current := t.pending[requestIDKey(params.RequestID)]; current != nil && !current.initialize {
			current.cancel()
			return current
		}
	}
	return nil
}

// CancelRequest cancels and drains a request before the broker changes protocol
// modes after a discovery timeout. Ordinary HTTP cancellation notifications use
// the same per-request context.
func (t *httpTransport) CancelRequest(id json.RawMessage) {
	t.mu.Lock()
	pending := t.pending[requestIDKey(id)]
	if pending != nil && !pending.initialize {
		pending.cancel()
	} else {
		pending = nil
	}
	t.mu.Unlock()
	if pending != nil {
		<-pending.done
	}
}

func (t *httpTransport) emit(ctx context.Context, message wire.Message, parent ...json.RawMessage) error {
	event := routedMessage{message: message}
	if len(parent) != 0 {
		event.parent = append(json.RawMessage(nil), parent[0]...)
	}
	select {
	case t.incoming <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.ctx.Done():
		return ErrClosed
	}
}

func (t *httpTransport) fail(err error) {
	t.mu.Lock()
	if t.failure == nil {
		t.failure = err
	}
	t.mu.Unlock()
	t.cancel()
}

func (t *httpTransport) postFailure(id json.RawMessage, request, initialize, discover bool, err error, terminal bool) {
	if t.ctx.Err() != nil {
		return
	}
	if initialize {
		t.mu.Lock()
		t.state = httpNew
		t.session, t.protocol, t.offeredProtocol = "", "", ""
		t.mu.Unlock()
	}
	if request {
		t.completePending(id)
		select {
		case t.incoming <- routedMessage{message: wire.Error(id, -32000, err.Error()), parent: append(json.RawMessage(nil), id...)}:
		case <-t.ctx.Done():
		default:
			// A cancelled/failed operation must release its lane even when the
			// receiver stopped reading. Terminate instead of silently losing its error.
			t.fail(errors.New("MCP HTTP receive queue is full; reconnect to create a new session"))
		}
	}
	if terminal || !request {
		t.fail(err)
	}
}

// newRequest is used only for legacy unsolicited GET and final DELETE. The SDK
// constructs every POST; its adapter adds configured headers and legacy state.
func (t *httpTransport) newRequest(ctx context.Context, method string, data []byte) (*http.Request, error) {
	var body io.Reader
	if data != nil {
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.endpoint, body)
	if err != nil {
		return nil, errors.New("cannot construct MCP HTTP request")
	}
	req.GetBody = nil
	req.Header = configuredHTTPHeaders(t.headers)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Del("Content-Type")
	t.mu.Lock()
	session, protocol := t.session, t.protocol
	t.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	if t.oauth != nil {
		source, err := t.oauth.TokenSource(ctx)
		if err != nil {
			return nil, errors.New("MCP OAuth token unavailable")
		}
		if source != nil {
			token, err := source.Token()
			if err != nil {
				return nil, errors.New("MCP OAuth token unavailable")
			}
			if token != nil {
				token.SetAuthHeader(req)
			}
		}
	}
	return req, nil
}

// MCP 2026 wraps non-ASCII, control, edge-whitespace and reserved sentinel
// values in =?base64?...?= to prevent header injection and preserve names.
func encodeMCPHeader(value string) string {
	unsafe := !utf8.ValidString(value) || strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=")
	if len(value) != len(strings.Trim(value, " \t")) {
		unsafe = true
	}
	for _, r := range value {
		if r < 32 || r > 126 || r == 127 {
			unsafe = true
			break
		}
	}
	if unsafe {
		return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
	}
	return value
}

func (t *httpTransport) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.cancel()
		t.mu.Unlock()
		t.wg.Wait()
		t.mu.Lock()
		session := t.session
		t.mu.Unlock()
		if session != "" {
			ctx, cancel := context.WithTimeout(context.Background(), httpCloseTimeout)
			req, err := t.newRequest(ctx, http.MethodDelete, nil)
			if err == nil {
				if response, err := t.client.Do(req); err == nil {
					_ = response.Body.Close()
				}
			}
			cancel()
		}
		t.network.CloseIdleConnections()
	})
	return nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			continue
		}
		return false
	}
	return true
}

func validHeaderValue(value string) bool {
	for _, c := range value {
		if (c < 32 && c != '\t') || c == 127 {
			return false
		}
	}
	return true
}
