package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agentmux/internal/config"
	"agentmux/internal/wire"
)

const (
	httpRequestLimit   = 8
	httpControlLimit   = 4
	httpRequestTimeout = 2 * time.Minute
	httpStreamLifetime = 5 * time.Minute
	httpCloseTimeout   = 2 * time.Second
)

var ErrBusy = errors.New("MCP HTTP transport concurrency limit reached")

type httpState uint8

const (
	httpNew httpState = iota
	httpInitializing
	httpNeedsInitialized
	httpSendingInitialized
	httpReady
)

type httpPending struct {
	cancel     context.CancelFunc
	initialize bool
}

type httpTransport struct {
	ctx             context.Context
	cancel          context.CancelFunc
	endpoint        string
	headers         http.Header
	client          *http.Client
	network         *http.Transport
	incoming        chan wire.Message
	requests        chan struct{}
	controls        chan struct{}
	ready           chan struct{}
	mu              sync.Mutex
	state           httpState
	session         string
	protocol        string
	offeredProtocol string
	closed          bool
	failure         error
	pending         map[string]*httpPending
	wg              sync.WaitGroup
	closeOnce       sync.Once
}

func startHTTP(parent context.Context, s config.Server) (Transport, error) {
	u, err := url.Parse(s.URL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("MCP HTTP URL must be an absolute http(s) endpoint without userinfo or fragment")
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
		MaxConnsPerHost:        httpRequestLimit + httpControlLimit + 1,
		MaxIdleConns:           httpRequestLimit + httpControlLimit + 1,
		MaxIdleConnsPerHost:    httpRequestLimit + httpControlLimit + 1,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  httpRequestTimeout,
		MaxResponseHeaderBytes: 64 << 10,
	}
	t := &httpTransport{ctx: ctx, cancel: cancel, endpoint: u.String(), headers: headers,
		network: network, incoming: make(chan wire.Message, 8), requests: make(chan struct{}, httpRequestLimit),
		controls: make(chan struct{}, httpControlLimit), ready: make(chan struct{}), pending: make(map[string]*httpPending)}
	t.client = &http.Client{Transport: network, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	go func() { <-ctx.Done(); _ = t.Close() }()
	return t, nil
}

func (t *httpTransport) PID() int { return 0 }

func (t *httpTransport) Read() (wire.Message, error) {
	select {
	case message := <-t.incoming:
		return message, nil
	default:
	}
	select {
	case message := <-t.incoming:
		return message, nil
	case <-t.ctx.Done():
		select {
		case message := <-t.incoming:
			return message, nil
		default:
		}
		t.mu.Lock()
		err := t.failure
		t.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
}

func isRequest(message wire.Message) bool { return message.Method() != "" && message.HasID() }

func (t *httpTransport) Write(message wire.Message) error {
	method := message.Method()
	request := isRequest(message)
	initialize := method == "initialize" && request
	initialized := method == "notifications/initialized" && !message.HasID()
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
	}

	t.mu.Lock()
	if t.closed || t.ctx.Err() != nil {
		t.mu.Unlock()
		return ErrClosed
	}
	if initialize && t.state != httpNew {
		t.mu.Unlock()
		return errors.New("MCP HTTP session is already initializing or initialized")
	}
	if initialized && t.state != httpNeedsInitialized {
		t.mu.Unlock()
		return errors.New("MCP HTTP initialize result must precede notifications/initialized")
	}
	// Callback responses must remain writable even during initialize, and must
	// not compete for slots held by SSE requests waiting for those callbacks.
	if !initialize && !initialized && method != "" && t.state < httpSendingInitialized {
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
	if initialize {
		t.state = httpInitializing
		t.offeredProtocol = offered.ProtocolVersion
	}
	if initialized {
		t.state = httpSendingInitialized
	}
	ctx, cancel := context.WithTimeout(t.ctx, httpRequestTimeout)
	pending := &httpPending{cancel: cancel, initialize: initialize}
	if request {
		t.pending[key] = pending
	}
	if method == "notifications/cancelled" {
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(message["params"], &params) == nil {
			if current := t.pending[requestIDKey(params.RequestID)]; current != nil && !current.initialize {
				current.cancel()
			}
		}
	}
	waitReady := !initialize && !initialized && method != "" && t.state != httpReady
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
		}()
		if waitReady {
			select {
			case <-t.ready:
			case <-ctx.Done():
				t.postFailure(id, request, initialize, requestError(ctx), false)
				return
			}
		}
		err := t.post(ctx, data, id, request, initialize, initialized)
		if err != nil {
			t.postFailure(id, request, initialize, err, errors.Is(err, errSessionExpired))
		}
	}()
	return nil
}

func (t *httpTransport) emit(ctx context.Context, message wire.Message) error {
	select {
	case t.incoming <- message:
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

func (t *httpTransport) postFailure(id json.RawMessage, request, initialize bool, err error, terminal bool) {
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
		case t.incoming <- wire.Error(id, -32000, err.Error()):
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

func (t *httpTransport) newRequest(ctx context.Context, method string, data []byte) (*http.Request, error) {
	var body io.Reader
	if data != nil {
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.endpoint, body)
	if err != nil {
		return nil, errors.New("cannot construct MCP HTTP request")
	}
	// Do not install GetBody: it can enable automatic replay of a POST after a
	// connection failure if an Idempotency-Key was supplied by configuration.
	req.GetBody = nil
	req.Header = t.headers.Clone()
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Del("Content-Type")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Del("Mcp-Session-Id")
	req.Header.Del("Mcp-Protocol-Version")
	req.Header.Del("Last-Event-Id")
	t.mu.Lock()
	session, protocol := t.session, t.protocol
	t.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	return req, nil
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
