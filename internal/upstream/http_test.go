package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/config"
	"agentcfg/internal/wire"
)

func writeHTTPJSON(w http.ResponseWriter, message wire.Message) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(message)
}

func writeHTTPEvent(w http.ResponseWriter, message wire.Message) {
	data, _ := json.Marshal(message)
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func decodeHTTP(t *testing.T, r *http.Request) wire.Message {
	t.Helper()
	defer r.Body.Close()
	var message wire.Message
	if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
		t.Errorf("decode fixture request: %v", err)
	}
	return message
}

func normalHTTPHandler(t *testing.T, call func(http.ResponseWriter, *http.Request, wire.Message)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
			w.WriteHeader(405)
			return
		}
		message := decodeHTTP(t, r)
		switch message.Method() {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "fixture-session")
			writeHTTPJSON(w, wire.Result(message.ID(), map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			call(w, r, message)
		}
	})
}

func newHTTPFixture(t *testing.T, handler http.Handler) *httpTransport {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	transport := startTestTransport(t, context.Background(), config.Server{Type: "streamable-http", URL: server.URL + "/mcp?token=url-secret",
		Headers: map[string]string{"Authorization": "Bearer header-secret", "X-Custom": "custom-value"}})
	return transport.(*httpTransport)
}

func initializeHTTP(t *testing.T, transport *httpTransport) {
	t.Helper()
	if err := transport.Write(rpcRequest("init", "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})); err != nil {
		t.Fatal(err)
	}
	if message := mustRead(t, transport); message["error"] != nil || message["result"] == nil {
		t.Fatalf("initialize failed: %s", message["error"])
	}
	if err := transport.Write(wire.Notification("notifications/initialized", nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("initialized was not accepted")
	}
}

func TestHTTPLiteralURLCredentialsReachEndpoint(t *testing.T) {
	handler := normalHTTPHandler(t, func(w http.ResponseWriter, r *http.Request, message wire.Message) {
		writeHTTPJSON(w, wire.Result(message.ID(), map[string]any{}))
	})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "fixture-user" || password != "fixture-password" || r.URL.Query().Get("token") != "fixture-token" {
			t.Error("configured URL credentials or query were lost")
		}
		requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	endpoint, _ := url.Parse(server.URL + "/mcp?token=fixture-token")
	endpoint.User = url.UserPassword("fixture-user", "fixture-password")
	transport := startTestTransport(t, context.Background(), config.Server{Type: "streamable-http", URL: endpoint.String()}).(*httpTransport)
	initializeHTTP(t, transport)
	if requests.Load() < 2 {
		t.Fatal("MCP handshake did not reach endpoint")
	}
}

func TestHTTPJSONSessionHeadersOrderingAndDelete(t *testing.T) {
	initializedSeen, releaseInitialized, callSeen, deleted := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	var initializeAccepted atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer header-secret" || r.Header.Get("X-Custom") != "custom-value" {
			t.Error("configured headers missing")
		}
		if r.URL.Path != "/mcp" {
			t.Error("endpoint path changed")
		}
		if r.Method == http.MethodPost {
			if r.Header.Get("Content-Type") != "application/json" || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
				t.Error("MCP content headers missing")
			}
			message := decodeHTTP(t, r)
			if message.Method() == "initialize" {
				if r.Header.Get("Mcp-Session-Id") != "" {
					t.Error("session sent before initialization")
				}
				w.Header().Set("Mcp-Session-Id", "session-abc")
				writeHTTPJSON(w, wire.Result(message.ID(), map[string]string{"protocolVersion": "2025-06-18"}))
				return
			}
			if r.Header.Get("Mcp-Session-Id") != "session-abc" || r.Header.Get("MCP-Protocol-Version") != "2025-06-18" {
				t.Error("session or negotiated protocol missing")
			}
			if message.Method() == "notifications/initialized" {
				close(initializedSeen)
				select {
				case <-releaseInitialized:
				case <-r.Context().Done():
					return
				}
				initializeAccepted.Store(true)
				w.WriteHeader(202)
				return
			}
			if !initializeAccepted.Load() {
				t.Error("tool POST overtook initialized")
			}
			close(callSeen)
			response := wire.Result(message.ID(), map[string]string{"ok": "yes"})
			response["id"] = json.RawMessage(`"\u0061bc"`)
			writeHTTPJSON(w, response)
			return
		}
		if r.Header.Get("Mcp-Session-Id") != "session-abc" || r.Header.Get("MCP-Protocol-Version") != "2025-06-18" {
			t.Error("GET/DELETE session headers missing")
		}
		if r.Method == http.MethodDelete {
			deleted <- struct{}{}
			w.WriteHeader(204)
		} else {
			w.WriteHeader(405)
		}
	})
	transport := newHTTPFixture(t, handler)
	if transport.PID() != 0 {
		t.Fatal("HTTP transport has a subprocess PID")
	}
	if err := transport.Write(rpcRequest("init", "initialize", map[string]string{"protocolVersion": "2025-11-25"})); err != nil {
		t.Fatal(err)
	}
	mustRead(t, transport)
	if err := transport.Write(wire.Notification("notifications/initialized", nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-initializedSeen:
	case <-time.After(time.Second):
		t.Fatal("initialized not sent")
	}
	if err := transport.Write(rpcRequest("abc", "tools/list", nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-callSeen:
		t.Fatal("tool sent before initialized acceptance")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseInitialized)
	message := mustRead(t, transport)
	if string(message.ID()) != `"abc"` || message["result"] == nil {
		t.Fatalf("response ID lost: %#v", message)
	}
	closeTransport(t, transport)
	select {
	case <-deleted:
	default:
		t.Fatal("DELETE not attempted")
	}
}

func TestHTTPSSECallbacksAndNotifications(t *testing.T) {
	callbackReplied := make(chan struct{})
	transport := newHTTPFixture(t, normalHTTPHandler(t, func(w http.ResponseWriter, r *http.Request, message wire.Message) {
		if message.Method() == "" {
			if string(message.ID()) != `"server-callback"` || message["result"] == nil {
				t.Error("callback response changed")
			}
			close(callbackReplied)
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, ": heartbeat\n\nid: cursor\ndata:\n\n")
		writeHTTPEvent(w, rpcRequest("server-callback", "ping", nil))
		select {
		case <-callbackReplied:
		case <-r.Context().Done():
			return
		}
		writeHTTPEvent(w, wire.Notification("notifications/progress", map[string]int{"progress": 1}))
		writeHTTPEvent(w, wire.Result(message.ID(), map[string]bool{"ok": true}))
	}))
	initializeHTTP(t, transport)
	if err := transport.Write(rpcRequest(7, "tools/call", nil)); err != nil {
		t.Fatal(err)
	}
	callback := mustRead(t, transport)
	if callback.Method() != "ping" {
		t.Fatal("server callback not forwarded")
	}
	if err := transport.Write(wire.Result(callback.ID(), map[string]any{})); err != nil {
		t.Fatal(err)
	}
	if message := mustRead(t, transport); message.Method() != "notifications/progress" {
		t.Fatal("progress notification lost")
	}
	if message := mustRead(t, transport); string(message.ID()) != "7" || message["result"] == nil {
		t.Fatal("SSE result lost")
	}
}

func TestHTTPInitializeSSECallbackUsesProvisionalSession(t *testing.T) {
	replied := make(chan struct{})
	transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		message := decodeHTTP(t, r)
		switch message.Method() {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "early-session")
			w.Header().Set("Content-Type", "text/event-stream")
			writeHTTPEvent(w, rpcRequest("init-ping", "ping", nil))
			select {
			case <-replied:
			case <-r.Context().Done():
				return
			}
			writeHTTPEvent(w, wire.Result(message.ID(), map[string]string{"protocolVersion": "2025-06-18"}))
		case "":
			if r.Header.Get("Mcp-Session-Id") != "early-session" || r.Header.Get("MCP-Protocol-Version") != "2025-11-25" {
				t.Error("initialization callback lacks provisional session/protocol")
			}
			close(replied)
			w.WriteHeader(202)
		case "notifications/initialized":
			if r.Header.Get("MCP-Protocol-Version") != "2025-06-18" {
				t.Error("negotiated protocol not installed")
			}
			w.WriteHeader(202)
		}
	}))
	if err := transport.Write(rpcRequest("init", "initialize", map[string]string{"protocolVersion": "2025-11-25"})); err != nil {
		t.Fatal(err)
	}
	callback := mustRead(t, transport)
	if callback.Method() != "ping" {
		t.Fatal("initialization callback lost")
	}
	if err := transport.Write(wire.Result(callback.ID(), map[string]any{})); err != nil {
		t.Fatal(err)
	}
	if result := mustRead(t, transport); result["result"] == nil || string(result.ID()) != `"init"` {
		t.Fatal("initialization result lost")
	}
	if err := transport.Write(wire.Notification("notifications/initialized", nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.ready:
	case <-time.After(time.Second):
		t.Fatal("initialized not accepted")
	}
}

func TestHTTPOptionalGETAndCloseCancellation(t *testing.T) {
	getEnded := make(chan struct{})
	base := normalHTTPHandler(t, func(w http.ResponseWriter, r *http.Request, message wire.Message) {
		writeHTTPJSON(w, wire.Result(message.ID(), true))
	})
	transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			base.ServeHTTP(w, r)
			return
		}
		defer close(getEnded)
		if r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("Mcp-Session-Id") != "fixture-session" {
			t.Error("GET headers missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeHTTPEvent(w, wire.Notification("notifications/tools/list_changed", nil))
		<-r.Context().Done()
	}))
	initializeHTTP(t, transport)
	if message := mustRead(t, transport); message.Method() != "notifications/tools/list_changed" {
		t.Fatal("unsolicited GET message lost")
	}
	if err := transport.Write(rpcRequest(10, "tools/list", nil)); err != nil {
		t.Fatal(err)
	}
	if message := mustRead(t, transport); message["result"] == nil {
		t.Fatal("POST failed while GET active")
	}
	closeTransport(t, transport)
	select {
	case <-getEnded:
	case <-time.After(time.Second):
		t.Fatal("GET did not stop on close")
	}
	if _, err := readResult(t, transport); err != io.EOF {
		t.Fatalf("closed read: %v", err)
	}
}

func TestHTTPFailuresAreCorrelatedSanitizedAndNotRetried(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	cases := []struct {
		name    string
		respond func(http.ResponseWriter)
	}{
		{"unauthorized", func(w http.ResponseWriter) { w.WriteHeader(401); _, _ = io.WriteString(w, "body-secret") }},
		{"status", func(w http.ResponseWriter) { w.WriteHeader(500); _, _ = io.WriteString(w, "body-secret") }},
		{"redirect", func(w http.ResponseWriter) {
			w.Header().Set("Location", target.URL+"?secret=body-secret")
			w.WriteHeader(307)
		}},
		{"malformed-json", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "not-json body-secret")
		}},
		{"oversized-json", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, strings.Repeat("x", wire.MaxFrameSize+2))
		}},
		{"malformed-sse", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: body-secret\n\n")
		}},
		{"sse-eof", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, ": heartbeat\n\n")
		}},
		{"accepted-without-response", func(w http.ResponseWriter) { w.WriteHeader(202) }},
		{"wrong-id", func(w http.ResponseWriter) { writeHTTPJSON(w, wire.Result(wire.Raw("somebody-else"), true)) }},
		{"content-type", func(w http.ResponseWriter) { _, _ = io.WriteString(w, "body-secret") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			transport := newHTTPFixture(t, normalHTTPHandler(t, func(w http.ResponseWriter, r *http.Request, _ wire.Message) { calls.Add(1); tc.respond(w) }))
			initializeHTTP(t, transport)
			if err := transport.Write(rpcRequest("original-id", "tools/call", nil)); err != nil {
				t.Fatal(err)
			}
			message := mustRead(t, transport)
			if string(message.ID()) != `"original-id"` || message["error"] == nil {
				t.Fatalf("missing correlated error: %#v", message)
			}
			encoded, _ := json.Marshal(message)
			for _, secret := range []string{"body-secret", "header-secret", "url-secret", "127.0.0.1"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("transport error leaked %s", secret)
				}
			}
			closeTransport(t, transport)
			if calls.Load() != 1 {
				t.Fatalf("POST replayed %d times", calls.Load())
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestHTTPSessionExpiredClosesTransport(t *testing.T) {
	transport := newHTTPFixture(t, normalHTTPHandler(t, func(w http.ResponseWriter, r *http.Request, _ wire.Message) { w.WriteHeader(404) }))
	initializeHTTP(t, transport)
	if err := transport.Write(rpcRequest(1, "tools/list", nil)); err != nil {
		t.Fatal(err)
	}
	if message := mustRead(t, transport); message["error"] == nil {
		t.Fatal("session expiration error missing")
	}
	if _, err := readResult(t, transport); !errors.Is(err, errSessionExpired) {
		t.Fatalf("session retained after 404: %v", err)
	}
	if err := transport.Write(rpcRequest(2, "tools/list", nil)); !errors.Is(err, ErrClosed) {
		t.Fatalf("expired transport accepted write: %v", err)
	}
}

func TestHTTPBoundedRequestsStillAcceptCallbackResponses(t *testing.T) {
	started := make(chan struct{}, httpRequestLimit)
	controlSeen := make(chan struct{})
	var release sync.Once
	transport := newHTTPFixture(t, normalHTTPHandler(t, func(w http.ResponseWriter, r *http.Request, message wire.Message) {
		if message.Method() == "" {
			release.Do(func() { close(controlSeen) })
			w.WriteHeader(202)
			return
		}
		started <- struct{}{}
		select {
		case <-controlSeen:
		case <-r.Context().Done():
			return
		}
		writeHTTPJSON(w, wire.Result(message.ID(), true))
	}))
	initializeHTTP(t, transport)
	// The initialized notification can mark the session ready while the
	// initialize POST still holds a request slot. Reserve the whole lane once
	// to wait for that cleanup before testing the capacity with real requests.
	func() {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		reserved := 0
		defer func() {
			for reserved > 0 {
				<-transport.requests
				reserved--
			}
		}()
		for reserved < cap(transport.requests) {
			select {
			case transport.requests <- struct{}{}:
				reserved++
			case <-timer.C:
				t.Fatal("initialize request did not release its concurrency slot")
			}
		}
	}()
	for i := 0; i < httpRequestLimit; i++ {
		if err := transport.Write(rpcRequest(i, "tools/call", nil)); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	for i := 0; i < httpRequestLimit; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("request lane did not fill")
		}
	}
	if err := transport.Write(rpcRequest(99, "tools/call", nil)); !errors.Is(err, ErrBusy) {
		t.Fatalf("request limit not enforced: %v", err)
	}
	if err := transport.Write(wire.Result(wire.Raw("callback"), map[string]any{})); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < httpRequestLimit; i++ {
		if message := mustRead(t, transport); message["result"] == nil {
			t.Fatal("request did not finish after control reply")
		}
	}
}

func TestHTTPCancelledNotificationStopsMatchingPOST(t *testing.T) {
	started, stopped, cancelled := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	transport := newHTTPFixture(t, normalHTTPHandler(t, func(w http.ResponseWriter, r *http.Request, message wire.Message) {
		if message.Method() == "notifications/cancelled" {
			cancelled <- struct{}{}
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	initializeHTTP(t, transport)
	if err := transport.Write(rpcRequest("abc", "tools/call", nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	message := wire.Notification("notifications/cancelled", nil)
	message["params"] = json.RawMessage(`{"requestId":"\u0061bc","reason":"test"}`)
	if err := transport.Write(message); err != nil {
		t.Fatal(err)
	}
	response := mustRead(t, transport)
	if string(response.ID()) != `"abc"` || response["error"] == nil {
		t.Fatal("cancelled request did not get its original ID error")
	}
	for _, signal := range []chan struct{}{stopped, cancelled} {
		select {
		case <-signal:
		case <-time.After(time.Second):
			t.Fatal("cancellation not propagated")
		}
	}
}

func TestHTTPContextTimeoutAndConcurrentClose(t *testing.T) {
	transport := newHTTPFixture(t, normalHTTPHandler(t, func(w http.ResponseWriter, r *http.Request, _ wire.Message) { <-r.Context().Done() }))
	initializeHTTP(t, transport)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	data, _ := json.Marshal(rpcRequest(12, "tools/call", nil))
	if err := transport.post(ctx, data, wire.Raw(12), true, false, false, false); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout not reported: %v", err)
	}
	var closers sync.WaitGroup
	for i := 0; i < 4; i++ {
		closers.Add(1)
		go func() { defer closers.Done(); _ = transport.Close() }()
	}
	closeTransport(t, transport)
	closers.Wait()
	if err := transport.Write(rpcRequest(2, "tools/list", nil)); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
}

func TestHTTPRejectsUnsafeEndpointsAndHeaders(t *testing.T) {
	for _, server := range []config.Server{
		{Type: "http", URL: "file:///tmp/mcp"},
		{Type: "http", URL: "https://example.com/mcp#fragment"},
		{Type: "http", URL: "http://localhost", Headers: map[string]string{"Host": "elsewhere"}},
		{Type: "http", URL: "http://localhost", Headers: map[string]string{"X-Test": "secret\r\nInjected:yes"}},
		{Type: "http", URL: "http://localhost", Headers: map[string]string{"authorization": "one", "Authorization": "two"}},
	} {
		if transport, err := Start(context.Background(), server, nil); err == nil {
			transport.Close()
			t.Error("invalid HTTP config accepted")
		}
	}
}

func TestHTTPGETBackpressureDoesNotSilentlyRotate(t *testing.T) {
	transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeHTTPEvent(w, wire.Notification("notifications/tools/list_changed", nil))
		<-r.Context().Done()
	}))
	for i := 0; i < cap(transport.incoming); i++ {
		transport.incoming <- routedMessage{message: wire.Notification("queued", nil)}
	}
	ctx, cancel := context.WithTimeout(transport.ctx, 40*time.Millisecond)
	defer cancel()
	_, err := transport.getStream(ctx)
	if !errors.Is(err, errUndeliveredEvent) {
		t.Fatalf("decoded event dropped as routine rotation: %v", err)
	}
}
