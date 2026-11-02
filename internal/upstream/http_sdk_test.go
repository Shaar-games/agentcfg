package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentmux/internal/auth"
	"agentmux/internal/wire"

	"golang.org/x/oauth2"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type testOAuthHandler struct {
	tokenSource func(context.Context) (oauth2.TokenSource, error)
	authorize   func(context.Context, *http.Request, *http.Response) error
}

func (h testOAuthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	return h.tokenSource(ctx)
}
func (h testOAuthHandler) Authorize(ctx context.Context, request *http.Request, response *http.Response) error {
	defer response.Body.Close()
	return h.authorize(ctx, request, response)
}

func readRoutedWithin(t *testing.T, transport *httpTransport) routedMessage {
	t.Helper()
	type result struct {
		message routedMessage
		err     error
	}
	done := make(chan result, 1)
	go func() {
		message, parent, err := transport.ReadRouted()
		done <- result{routedMessage{message: message, parent: parent}, err}
	}()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result.message
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading routed SDK message")
		return routedMessage{}
	}
}

func TestSDKHTTPProtocolErrorsPreserveCodeDataAndMode(t *testing.T) {
	for _, tc := range []struct {
		code, status int
		modern       bool
	}{
		{-32022, 400, true}, {-32021, 400, true}, {-32020, 400, true}, {-32601, 404, false},
	} {
		t.Run(http.StatusText(tc.status)+"/"+string(wire.Raw(tc.code)), func(t *testing.T) {
			transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				message := decodeHTTP(t, r)
				answer := wire.Error(message.ID(), tc.code, "protocol rejection")
				answer["error"] = wire.Raw(map[string]any{"code": tc.code, "message": "protocol rejection", "data": map[string]any{"supportedVersions": []string{wire.ModernProtocolVersion}}})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(answer)
			}))
			if err := transport.Write(modernHTTPMessage("probe", "server/discover", nil)); err != nil {
				t.Fatal(err)
			}
			answer := mustRead(t, transport)
			if rpcErrorCode(answer) != tc.code || !strings.Contains(string(answer["error"]), "supportedVersions") {
				t.Fatalf("protocol error was flattened: %s", answer["error"])
			}
			transport.mu.Lock()
			state := transport.state
			transport.mu.Unlock()
			if tc.modern && state != httpModern || !tc.modern && state != httpNew {
				t.Fatalf("wrong fallback state %v", state)
			}
		})
	}
}

func TestSDKHTTPDiscoveryCancellationDrainsBeforeFallback(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	base := normalHTTPHandler(t, func(w http.ResponseWriter, r *http.Request, message wire.Message) {
		if message.Method() != "server/discover" {
			t.Error("unexpected fixture method")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	})
	transport := newHTTPFixture(t, base)
	if err := transport.Write(modernHTTPMessage("probe", "server/discover", nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	transport.CancelRequest(wire.Raw("probe"))
	if answer := mustRead(t, transport); answer["error"] == nil {
		t.Fatal("cancelled probe has no correlated result")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("probe HTTP context still alive")
	}
	initializeHTTP(t, transport)
}

func TestSDKHTTPSubscriptionOriginAndSeparateCapacity(t *testing.T) {
	var unexpected, cancelled atomic.Int32
	transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			unexpected.Add(1)
			w.WriteHeader(405)
			return
		}
		message := decodeHTTP(t, r)
		switch message.Method() {
		case "subscriptions/listen":
			w.Header().Set("Content-Type", "text/event-stream")
			// A session header on a modern response must never establish a session.
			w.Header().Set("Mcp-Session-Id", "must-be-ignored")
			writeHTTPEvent(w, wire.Notification("notifications/subscriptions/acknowledged", map[string]any{
				"_meta": map[string]any{"io.modelcontextprotocol/subscriptionId": json.RawMessage(message.ID())},
			}))
			<-r.Context().Done()
			cancelled.Add(1)
		case "tools/list":
			writeHTTPJSON(w, wire.Result(message.ID(), map[string]any{"tools": []any{}}))
		default:
			unexpected.Add(1)
			w.WriteHeader(400)
		}
	}))
	base := transport.client.Transport
	transport.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Mcp-Method") == "subscriptions/listen" {
			if _, hasDeadline := request.Context().Deadline(); hasDeadline {
				t.Error("subscription inherited ordinary RPC deadline")
			}
		}
		return base.RoundTrip(request)
	})
	for id := 0; id < httpSubscriptionLimit; id++ {
		if err := transport.Write(modernHTTPMessage(id, "subscriptions/listen", map[string]any{"subscriptions": map[string]any{}})); err != nil {
			t.Fatal(err)
		}
		event := readRoutedWithin(t, transport)
		if string(event.parent) != string(wire.Raw(id)) || event.message.Method() != "notifications/subscriptions/acknowledged" {
			t.Fatalf("subscription notification lost POST origin: %#v", event)
		}
	}
	if err := transport.Write(modernHTTPMessage("extra", "subscriptions/listen", nil)); !errors.Is(err, ErrBusy) {
		t.Fatalf("subscription capacity not bounded: %v", err)
	}
	if err := transport.Write(modernHTTPMessage("ordinary", "tools/list", nil)); err != nil {
		t.Fatalf("subscriptions starved ordinary request: %v", err)
	}
	result := readRoutedWithin(t, transport)
	if string(result.parent) != `"ordinary"` || result.message["result"] == nil {
		t.Fatalf("ordinary result: %#v", result)
	}
	transport.CancelRequest(wire.Raw(0))
	cancelResult := readRoutedWithin(t, transport)
	if string(cancelResult.parent) != "0" || cancelResult.message["error"] == nil {
		t.Fatalf("cancelled subscription result: %#v", cancelResult)
	}
	closeTransport(t, transport)
	if unexpected.Load() != 0 {
		t.Fatalf("modern GET/DELETE/control POST appeared: %d", unexpected.Load())
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for cancelled.Load() != httpSubscriptionLimit {
		select {
		case <-deadline.C:
			t.Fatalf("only %d subscriptions closed", cancelled.Load())
		case <-time.After(time.Millisecond):
		}
	}
}

func TestSDKHTTPMRTRAndTaskPayloadsAreRaw(t *testing.T) {
	expected := map[string]json.RawMessage{
		"tools/call": json.RawMessage(`{"resultType":"input_required","requestState":"opaque/keep","inputRequests":{"sample":{"method":"sampling/createMessage","params":{"futureField":true}}},"extension":{"nested":[1,2]}}`),
		"tasks/get":  json.RawMessage(`{"task":{"taskId":"opaque-task","status":"working"},"futureField":{"supportedByServer":true}}`),
	}
	transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		message := decodeHTTP(t, r)
		answer := wire.Result(message.ID(), nil)
		answer["result"] = expected[message.Method()]
		writeHTTPJSON(w, answer)
	}))
	for method, result := range expected {
		params := map[string]any{"name": "run", "requestState": "resume-state", "inputResponses": map[string]any{"sample": map[string]any{"accepted": true}}}
		if err := transport.Write(modernHTTPMessage(method, method, params)); err != nil {
			t.Fatal(err)
		}
		response := mustRead(t, transport)
		if string(response["result"]) != string(result) {
			t.Fatalf("raw %s result changed: %s", method, response["result"])
		}
	}
}

func TestSDKHTTPOAuthChallengeAndRequestCancellation(t *testing.T) {
	t.Run("challenge", func(t *testing.T) {
		var authorized atomic.Bool
		var posts, flows atomic.Int32
		transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			posts.Add(1)
			message := decodeHTTP(t, r)
			if r.Header.Get("Authorization") != "Bearer sdk-token" {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://unused.invalid/meta"`)
				w.WriteHeader(401)
				return
			}
			writeHTTPJSON(w, wire.Result(message.ID(), map[string]any{"tools": []any{}}))
		}))
		transport.oauth = testOAuthHandler{
			tokenSource: func(context.Context) (oauth2.TokenSource, error) {
				if !authorized.Load() {
					return nil, nil
				}
				return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "sdk-token", TokenType: "Bearer"}), nil
			},
			authorize: func(context.Context, *http.Request, *http.Response) error {
				flows.Add(1)
				authorized.Store(true)
				return nil
			},
		}
		if err := transport.Write(modernHTTPMessage("auth", "tools/list", nil)); err != nil {
			t.Fatal(err)
		}
		if response := mustRead(t, transport); response["error"] != nil {
			t.Fatal(response)
		}
		if posts.Load() != 2 || flows.Load() != 1 {
			t.Fatalf("SDK OAuth retry counts %d/%d", posts.Load(), flows.Load())
		}
	})
	t.Run("token-context", func(t *testing.T) {
		started := make(chan struct{})
		transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("cancelled auth reached endpoint") }))
		transport.oauth = testOAuthHandler{
			tokenSource: func(ctx context.Context) (oauth2.TokenSource, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			},
			authorize: func(context.Context, *http.Request, *http.Response) error { return auth.ErrLoginRequired },
		}
		if err := transport.Write(modernHTTPMessage("refresh", "tools/list", nil)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("token source did not start")
		}
		done := make(chan struct{})
		go func() { transport.CancelRequest(wire.Raw("refresh")); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("SDK OAuth cancellation deadlocked Close")
		}
		if response := mustRead(t, transport); response["error"] == nil {
			t.Fatal("OAuth cancellation unreported")
		}
	})
}

func TestSDKHTTPHeaderMismatchRefreshesOnlyRejectedCall(t *testing.T) {
	var calls, lists atomic.Int32
	transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		message := decodeHTTP(t, r)
		switch message.Method() {
		case "tools/list":
			lists.Add(1)
			writeHTTPJSON(w, wire.Result(message.ID(), map[string]any{"tools": []any{map[string]any{
				"name": "annotated", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"region": map[string]any{"type": "string", "x-mcp-header": "Region"}}},
			}}}))
		case "tools/call":
			calls.Add(1)
			if r.Header.Get("Mcp-Param-Region") != "west" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(wire.Error(message.ID(), -32020, "HeaderMismatch"))
				return
			}
			writeHTTPJSON(w, wire.Result(message.ID(), map[string]bool{"ok": true}))
		default:
			t.Errorf("unexpected refresh method %s", message.Method())
		}
	}))
	if err := transport.Write(modernHTTPMessage("cold", "tools/call", map[string]any{"name": "annotated", "arguments": map[string]any{"region": "west"}})); err != nil {
		t.Fatal(err)
	}
	if answer := mustRead(t, transport); answer["error"] != nil {
		t.Fatal(answer)
	}
	if calls.Load() != 2 || lists.Load() != 1 {
		t.Fatalf("refresh calls=%d lists=%d", calls.Load(), lists.Load())
	}
}
