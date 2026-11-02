package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"
	"unicode/utf8"

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

func (t *httpTransport) post(ctx context.Context, data []byte, id json.RawMessage, request, initialize, initialized bool) error {
	req, err := t.newRequest(ctx, http.MethodPost, data)
	if err != nil {
		return err
	}
	response, err := t.client.Do(req)
	if err != nil {
		return requestError(ctx)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound && req.Header.Get("Mcp-Session-Id") != "" {
		return errSessionExpired
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("MCP HTTP request returned status %d; response details omitted", response.StatusCode)
	}
	if !request {
		if response.StatusCode != http.StatusAccepted {
			return errors.New("MCP HTTP notification/response requires status 202")
		}
		if initialized {
			t.markReady()
		}
		return nil
	}
	if response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusNoContent {
		return errors.New("MCP HTTP request was accepted without a JSON-RPC response")
	}
	if initialize {
		// A callback can arrive on the initialization SSE stream before its
		// InitializeResult; that callback's response already needs this session.
		session := response.Header.Get("Mcp-Session-Id")
		if session != "" && !visibleASCII(session, 4096) {
			return errors.New("MCP HTTP initialize response has an invalid session ID")
		}
		t.mu.Lock()
		t.session, t.protocol = session, t.offeredProtocol
		t.mu.Unlock()
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		return errors.New("MCP HTTP response has an invalid content type")
	}
	matched := false
	receive := func(message wire.Message) error {
		if message.Method() == "" && message.HasID() && requestIDKey(message.ID()) == requestIDKey(id) {
			if message["result"] == nil && message["error"] == nil {
				return errors.New("MCP response lacks a result or error")
			}
			if initialize {
				if err := t.initializeResult(message, response.Header.Get("Mcp-Session-Id")); err != nil {
					return err
				}
			}
			matched = true
			t.completePending(id)
			message["id"] = append(json.RawMessage(nil), id...)
			if err := t.emit(ctx, message); err != nil {
				return err
			}
			return errStreamComplete
		}
		if message.Method() == "" {
			return errors.New("MCP HTTP response ID does not match its request")
		}
		return t.emit(ctx, message)
	}
	switch contentType {
	case "application/json":
		payload, err := io.ReadAll(io.LimitReader(response.Body, wire.MaxFrameSize+1))
		if err != nil {
			return requestError(ctx)
		}
		if !utf8.Valid(payload) {
			return errors.New("MCP HTTP message is not valid UTF-8")
		}
		message, err := wire.Decode(payload)
		if err != nil {
			return errors.New("invalid or oversized MCP HTTP JSON-RPC response")
		}
		err = receive(message)
		if err != nil && !errors.Is(err, errStreamComplete) {
			return err
		}
	case "text/event-stream":
		err := readSSE(response.Body, receive)
		if err != nil && !errors.Is(err, errStreamComplete) {
			if ctx.Err() != nil {
				return requestError(ctx)
			}
			return err
		}
	default:
		return errors.New("MCP HTTP response must be application/json or text/event-stream")
	}
	if !matched {
		return errors.New("MCP HTTP stream ended before its JSON-RPC response; the operation is not retried")
	}
	return nil
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
