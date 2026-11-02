package upstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"agentmux/internal/wire"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// A raw SDK Connection keeps arbitrary MCP methods and payloads intact. One
// connection per POST also preserves the request owning each SSE notification;
// the SDK otherwise merges concurrent response streams into one incoming queue.
// Legacy session ownership stays with httpTransport, so closing a request must
// not delete the backend's session.
func (t *httpTransport) sdkExchange(ctx context.Context, outgoing wire.Message, receive func(wire.Message) error) error {
	message, err := decodeSDKMessage(outgoing)
	if err != nil {
		return err
	}
	adapter := &sdkHTTPAdapter{
		transport: t, base: t.client.Transport, outgoing: outgoing,
		modern:     wire.IsModernRequest(outgoing),
		initialize: outgoing.Method() == "initialize" && outgoing.HasID(),
	}
	client := *t.client
	client.Transport = adapter
	var oauth sdkauth.OAuthHandler
	if t.oauth != nil {
		oauth = requestOAuthHandler{ctx: ctx, handler: t.oauth}
	}
	sdk := &mcp.StreamableClientTransport{
		Endpoint: t.endpoint, HTTPClient: &client, OAuthHandler: oauth,
		DisableStandaloneSSE: true, MaxRetries: -1, MaxEventSize: wire.MaxFrameSize,
	}
	connection, err := sdk.Connect(ctx)
	if err != nil {
		return err
	}
	// Connect detaches its lifetime context. Explicitly cancel it even when
	// OAuth token refresh or an asynchronous JSON/SSE reader is still active.
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer func() {
		stop()
		_ = connection.Close()
		adapter.closeBodies()
	}()
	if err := connection.Write(ctx, message); err != nil {
		// ErrRejected is itself a SDK *jsonrpc.Error marker, even for a
		// timeout or network failure. Only an ID-validated HTTP response
		// constitutes a remote protocol error; errors.As alone is unsafe.
		authFailure := t.oauth != nil && (adapter.status == 401 || adapter.status == 403)
		if adapter.protocolError != nil && isRequest(outgoing) && ctx.Err() == nil && !authFailure {
			return receive(adapter.protocolError)
		}
		return &sdkExchangeError{status: adapter.status, err: err}
	}
	if !isRequest(outgoing) {
		return nil
	}
	for {
		message, err := connection.Read(ctx)
		if err != nil {
			return err
		}
		incoming, err := encodeSDKMessage(message)
		if err != nil {
			return err
		}
		if err := receive(incoming); err != nil {
			return err
		}
	}
}

// The SDK asks TokenSource for its detached connection context while holding
// its session mutex. Bind token refresh to the original request so cancellation
// can release that mutex before Close tries to acquire it.
type requestOAuthHandler struct {
	ctx     context.Context
	handler sdkauth.OAuthHandler
}

func (h requestOAuthHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return h.handler.TokenSource(h.ctx)
}

func (h requestOAuthHandler) Authorize(_ context.Context, request *http.Request, response *http.Response) error {
	return h.handler.Authorize(h.ctx, request, response)
}

// The SDK's public raw connection has no sessionUpdated or Tool context hook.
// This HTTP decorator supplies that missing broker state, enforces the frame
// budget for JSON/error bodies, and prevents redirects/replay. POST framing,
// JSON-RPC parsing, SSE and OAuth remain the SDK's responsibility.
type sdkHTTPAdapter struct {
	transport     *httpTransport
	base          http.RoundTripper
	outgoing      wire.Message
	modern        bool
	initialize    bool
	protocolError wire.Message
	status        int
	bodies        []io.Closer // RoundTrip is serial for this single SDK Write.
}

type sdkExchangeError struct {
	status int
	err    error
}

func (e *sdkExchangeError) Error() string { return "MCP SDK HTTP exchange failed" }
func (e *sdkExchangeError) Unwrap() error { return e.err }

func (a *sdkHTTPAdapter) closeBodies() {
	for _, body := range a.bodies {
		_ = body.Close()
	}
}

func (a *sdkHTTPAdapter) RoundTrip(request *http.Request) (*http.Response, error) {
	t := a.transport
	a.status, a.protocolError = 0, nil
	endpoint, _ := url.Parse(t.endpoint)
	if request.Method != http.MethodPost || request.URL.Scheme != endpoint.Scheme ||
		!strings.EqualFold(request.URL.Host, endpoint.Host) || request.URL.String() != endpoint.String() {
		return nil, errors.New("MCP SDK attempted an unsupported HTTP replay or endpoint change")
	}
	request = request.Clone(request.Context())
	request.GetBody = nil
	headers := configuredHTTPHeaders(t.headers)
	// SDK-generated protocol, method, name and OAuth headers take precedence.
	for key, values := range request.Header {
		headers[key] = append([]string(nil), values...)
	}
	request.Header = headers
	t.mu.Lock()
	session, protocol := t.session, t.protocol
	t.mu.Unlock()
	if a.modern {
		request.Header.Del("Mcp-Session-Id")
		if names, present := request.Header["Mcp-Name"]; present && len(names) != 0 {
			// v1.8.0 extractName supplies the unescaped name. Apply the modern
			// transport's header-value encoding without changing the JSON body.
			request.Header.Set("Mcp-Name", encodeMCPHeader(names[0]))
		}
		if err := t.addToolParameterHeaders(request, a.outgoing); err != nil {
			return nil, err
		}
	} else {
		if session != "" {
			request.Header.Set("Mcp-Session-Id", session)
		}
		if protocol != "" {
			request.Header.Set("MCP-Protocol-Version", protocol)
		}
	}
	base := a.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	a.bodies = append(a.bodies, response.Body)
	a.status = response.StatusCode
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// SDK v1.8.0 otherwise reads error bodies with unbounded io.ReadAll and
		// does not check their response ID. Bound and validate before handing
		// the body back to its official error decoding path.
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, wire.MaxFrameSize+1))
		_ = response.Body.Close()
		if readErr != nil || len(payload) > wire.MaxFrameSize {
			return nil, errors.New("invalid or oversized MCP HTTP error response")
		}
		if contentType == "application/json" && utf8.Valid(payload) {
			if decoded, err := jsonrpc.DecodeMessage(payload); err == nil {
				if message, err := encodeSDKMessage(decoded); err == nil && message["error"] != nil {
					if requestIDKey(message.ID()) != requestIDKey(a.outgoing.ID()) {
						return nil, errors.New("MCP HTTP error response ID does not match its request")
					}
					a.protocolError = message
				}
			}
		}
		if response.StatusCode == http.StatusNotFound && session != "" && !a.modern && a.protocolError == nil {
			return nil, errSessionExpired
		}
		if a.protocolError == nil {
			// Unstructured bodies can contain credentials or diagnostic dumps.
			payload = nil
		}
		response.Body = io.NopCloser(bytes.NewReader(payload))
		response.ContentLength = int64(len(payload))
	} else if isRequest(a.outgoing) && (response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusNoContent) {
		_ = response.Body.Close()
		return nil, errors.New("MCP HTTP request was accepted without a JSON-RPC response")
	} else if !isRequest(a.outgoing) && response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusNoContent {
		_ = response.Body.Close()
		return nil, errors.New("MCP HTTP notification/response requires status 202 or 204")
	} else if contentType == "application/json" {
		payload, err := io.ReadAll(io.LimitReader(response.Body, wire.MaxFrameSize+1))
		_ = response.Body.Close()
		if err != nil || len(payload) > wire.MaxFrameSize || !utf8.Valid(payload) {
			return nil, errors.New("invalid or oversized MCP HTTP JSON response")
		}
		response.Body = io.NopCloser(bytes.NewReader(payload))
		response.ContentLength = int64(len(payload))
	}
	if a.initialize && response.StatusCode >= 200 && response.StatusCode < 300 {
		session = response.Header.Get("Mcp-Session-Id")
		if session != "" && !visibleASCII(session, 4096) {
			_ = response.Body.Close()
			return nil, errors.New("MCP HTTP initialize response has an invalid session ID")
		}
		t.mu.Lock()
		t.session, t.protocol = session, t.offeredProtocol
		t.mu.Unlock()
	}
	// Closing this SDK connection must never DELETE the shared legacy session.
	// In 2026 a stray response session header is ignored entirely.
	response.Header = response.Header.Clone()
	response.Header.Del("Mcp-Session-Id")
	return response, nil
}

func configuredHTTPHeaders(configured http.Header) http.Header {
	headers := configured.Clone()
	for key := range headers {
		lower := strings.ToLower(key)
		if lower == "mcp-session-id" || lower == "mcp-protocol-version" ||
			lower == "mcp-method" || lower == "mcp-name" || lower == "last-event-id" ||
			strings.HasPrefix(lower, "mcp-param-") {
			delete(headers, key)
		}
	}
	return headers
}
