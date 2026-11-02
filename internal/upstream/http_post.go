package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"time"

	"agentmux/internal/auth"
	"agentmux/internal/wire"
)

var errSessionExpired = errors.New("MCP HTTP session expired; reconnect and initialize a new session")
var errUndeliveredEvent = errors.New("MCP HTTP unsolicited message could not be delivered; reconnect to create a new session")

func requestError(ctx context.Context) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return errors.New("MCP HTTP request timed out; the operation is not retried")
	case ctx.Err() != nil:
		return errors.New("MCP HTTP request cancelled; remote execution may already have started")
	default:
		return errors.New("MCP HTTP request failed; the operation is not retried")
	}
}

func (t *httpTransport) post(ctx context.Context, data []byte, id json.RawMessage, request, initialize, initialized, discover bool) error {
	var outgoing wire.Message
	if json.Unmarshal(data, &outgoing) != nil {
		return errors.New("invalid MCP HTTP message")
	}
	modern := wire.IsModernRequest(outgoing)
	retriedHeaders := false
	var headerMismatch wire.Message
	receive := func(message wire.Message) error {
		if message.Method() == "" && message.HasID() && requestIDKey(message.ID()) == requestIDKey(id) {
			if modern && outgoing.Method() == "tools/call" && !retriedHeaders && rpcErrorCode(message) == -32020 {
				headerMismatch = message
				return errRefreshToolHeaders
			}
			if initialize {
				t.mu.Lock()
				session := t.session
				t.mu.Unlock()
				if err := t.initializeResult(message, session); err != nil {
					return err
				}
			}
			if discover && message["error"] != nil && !modernProtocolError(message) {
				t.resetFailedDiscovery()
			} else if modern {
				t.mu.Lock()
				t.modernEstablished = true
				t.mu.Unlock()
			}
			if modern && outgoing.Method() == "tools/list" && message["result"] != nil {
				t.captureToolDefinitions(message, outgoing)
			}
			t.completePending(id)
			message["id"] = append(json.RawMessage(nil), id...)
			if err := t.emit(ctx, message, id); err != nil {
				return err
			}
			return errStreamComplete
		}
		if message.Method() == "" {
			return errors.New("MCP HTTP response ID does not match its request")
		}
		if modern && message.HasID() {
			return errors.New("MCP 2026 server requests require multi-round-trip results")
		}
		return t.emit(ctx, message, id)
	}
	err := t.sdkExchange(ctx, outgoing, receive)
	if errors.Is(err, errRefreshToolHeaders) {
		// HeaderMismatch is a protocol rejection before execution. The spec
		// prescribes refreshing tools/list and retrying once with new headers.
		// No other tool failure causes an automatic replay.
		retriedHeaders = true
		if refreshErr := t.refreshToolHeaders(ctx, outgoing); refreshErr == nil {
			err = t.sdkExchange(ctx, outgoing, receive)
		} else if ctx.Err() == nil {
			err = receive(headerMismatch)
		} else {
			err = ctx.Err()
		}
	}
	if errors.Is(err, errStreamComplete) {
		return nil
	}
	if err == nil {
		if initialized {
			t.markReady()
		}
		return nil
	}
	if discover {
		var failed *sdkExchangeError
		if ctx.Err() != nil || errors.As(err, &failed) &&
			(failed.status == 400 || failed.status == 404 || failed.status == 405 || failed.status == 501) {
			t.resetFailedDiscovery()
		}
	}
	if errors.Is(err, errSessionExpired) {
		return errSessionExpired
	}
	if errors.Is(err, auth.ErrLoginRequired) {
		return auth.ErrLoginRequired
	}
	// SDK errors may contain a URL, session ID, invalid response bytes or auth
	// diagnostics. Only validated JSON-RPC errors are exposed verbatim above.
	return requestError(ctx)
}

func modernProtocolError(message wire.Message) bool {
	code := rpcErrorCode(message)
	return code == -32020 || code == -32021 || code == -32022
}

func rpcErrorCode(message wire.Message) int {
	var value struct{ Code int }
	if json.Unmarshal(message["error"], &value) != nil {
		return 0
	}
	return value.Code
}

// Modern capability probing is optional. A legacy server can reply with
// MethodNotFound, after which clients may retry using initialize. Do not reset
// an HTTP transport that already has other modern requests in flight.
func (t *httpTransport) resetFailedDiscovery() {
	t.mu.Lock()
	if t.state == httpModern && !t.modernEstablished && len(t.pending) <= 1 {
		t.state = httpNew
	}
	t.mu.Unlock()
}

func (t *httpTransport) completePending(id json.RawMessage) {
	t.mu.Lock()
	delete(t.pending, requestIDKey(id))
	t.mu.Unlock()
}

func (t *httpTransport) initializeResult(message wire.Message, session string) error {
	if message["error"] != nil {
		t.mu.Lock()
		t.state = httpNew
		t.session, t.protocol, t.offeredProtocol = "", "", ""
		t.mu.Unlock()
		return nil
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(message["result"], &result) != nil || !visibleASCII(result.ProtocolVersion, 128) {
		return errors.New("MCP HTTP initialize result has no valid protocolVersion")
	}
	if session != "" && !visibleASCII(session, 4096) {
		return errors.New("MCP HTTP initialize response has an invalid session ID")
	}
	t.mu.Lock()
	t.session, t.protocol = session, result.ProtocolVersion
	t.state = httpNeedsInitialized
	t.mu.Unlock()
	return nil
}

func visibleASCII(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, c := range value {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func (t *httpTransport) markReady() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.ctx.Err() != nil || t.state != httpSendingInitialized {
		return
	}
	t.state = httpReady
	close(t.ready)
	t.wg.Add(1)
	go func() { defer t.wg.Done(); t.getLoop() }()
}

func (t *httpTransport) getLoop() {
	for t.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(t.ctx, httpStreamLifetime)
		unsupported, err := t.getStream(ctx)
		cancel()
		if errors.Is(err, errSessionExpired) || errors.Is(err, errUndeliveredEvent) {
			t.fail(err)
			return
		}
		// A server is allowed to omit GET streaming. A malformed or refused GET
		// disables this optional channel while POST requests remain available.
		if unsupported || (err != nil && !errors.Is(err, context.DeadlineExceeded)) {
			return
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-t.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (t *httpTransport) getStream(ctx context.Context) (bool, error) {
	req, err := t.newRequest(ctx, http.MethodGet, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	response, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, errors.New("optional MCP HTTP GET failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound && req.Header.Get("Mcp-Session-Id") != "" {
		return false, errSessionExpired
	}
	if response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented {
		return true, nil
	}
	if response.StatusCode != http.StatusOK {
		return true, nil
	}
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if contentType != "text/event-stream" {
		return true, nil
	}
	deliveryFailed := false
	err = readSSE(response.Body, func(message wire.Message) error {
		// A fresh GET is for unsolicited requests/notifications, not responses to
		// unrelated POSTs. Stream resumption is deliberately not implemented.
		if message.Method() == "" {
			return errors.New("unexpected response on unsolicited MCP GET stream")
		}
		if err := t.emit(ctx, message); err != nil {
			deliveryFailed = t.ctx.Err() == nil
			return err
		}
		return nil
	})
	if deliveryFailed {
		return false, errUndeliveredEvent
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, err
}
