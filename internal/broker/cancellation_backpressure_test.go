package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentcfg/internal/config"
	"agentcfg/internal/upstream"
	"agentcfg/internal/wire"
)

type cancellationWriteResult struct {
	requestID string
	err       error
}

// Observe the real HTTP transport's admission result without changing its
// concurrency, request routing, cancellation or error behavior.
type observedCancellationHTTP struct {
	upstream.RoutedTransport
	writes chan cancellationWriteResult
}

func (o *observedCancellationHTTP) Write(message wire.Message) error {
	err := o.RoutedTransport.Write(message)
	if message.Method() == "notifications/cancelled" {
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(message["params"], &params)
		o.writes <- cancellationWriteResult{requestID: string(params.RequestID), err: err}
	}
	return err
}

type failedCancellationTransport struct{ *fakeTransport }

func (f *failedCancellationTransport) Write(message wire.Message) error {
	if message.Method() == "notifications/cancelled" {
		return fmt.Errorf("fixture cancellation write: %w", io.ErrClosedPipe)
	}
	return f.fakeTransport.Write(message)
}

func TestCancellationWriteFailureClosesSharedClients(t *testing.T) {
	transport := &failedCancellationTransport{fakeTransport: newFake()}
	b := newBackend(context.Background(), config.Server{Name: "fatal-write", Sharing: "shared"}, transport, time.Minute)
	t.Cleanup(b.close)
	a, other := attachTestPeer(t, b), attachTestPeer(t, b)
	initializePeer(t, b, a)
	initializePeer(t, b, other)
	b.handle(a.peer, toolRequest(10, "pending"))
	_ = nextWrite(t, transport.fakeTransport)
	b.handle(a.peer, wire.Notification("notifications/cancelled", map[string]any{"requestId": 10}))
	select {
	case <-b.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("fatal cancellation write error did not stop the shared backend")
	}
	for _, p := range []testPeer{a, other} {
		select {
		case <-p.peer.done:
		case <-time.After(5 * time.Second):
			t.Fatal("fatal cancellation write error left a client attached")
		}
	}
	select {
	case <-transport.done:
	case <-time.After(5 * time.Second):
		t.Fatal("fatal cancellation write error did not close its transport")
	}
}

func TestHTTPCancellationBackpressurePreservesSharedClients(t *testing.T) {
	controls := make(chan string, 8)
	calls := make(chan wire.Message, 8)
	releaseControls, releaseCalls := make(chan struct{}), make(chan struct{})
	var controlsOnce, callsOnce sync.Once
	unblockControls := func() { controlsOnce.Do(func() { close(releaseControls) }) }
	unblockCalls := func() { callsOnce.Do(func() { close(releaseCalls) }) }
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		defer r.Body.Close()
		var message wire.Message
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			t.Errorf("decode HTTP fixture request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch message.Method() {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "cancellation-backpressure")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(wire.Result(message.ID(), map[string]any{
				"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
				"serverInfo": map[string]string{"name": "backpressure-fixture", "version": "1"},
			}))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "notifications/cancelled":
			var params struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if err := json.Unmarshal(message["params"], &params); err != nil {
				t.Errorf("decode cancellation: %v", err)
				return
			}
			controls <- string(params.RequestID)
			select {
			case <-releaseControls:
				w.WriteHeader(http.StatusAccepted)
			case <-r.Context().Done():
			}
		case "tools/call":
			calls <- message
			select {
			case <-releaseCalls:
				var params struct{ Name string }
				if err := json.Unmarshal(message["params"], &params); err != nil {
					t.Errorf("decode tool call: %v", err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(wire.Result(message.ID(), map[string]any{
					"content": []map[string]string{{"type": "text", "text": params.Name}},
				}))
			case <-r.Context().Done():
			}
		default:
			t.Errorf("unexpected fixture method %q", message.Method())
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(func() { unblockControls(); unblockCalls(); fixture.Close() })
	server := config.Server{Name: "backpressure", Type: "streamable-http", URL: fixture.URL, Sharing: "shared"}
	transport, err := upstream.Start(context.Background(), server, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	routed, ok := transport.(upstream.RoutedTransport)
	if !ok {
		t.Fatal("HTTP transport does not preserve request routing")
	}
	observed := &observedCancellationHTTP{RoutedTransport: routed, writes: make(chan cancellationWriteResult, 32)}
	b := newBackend(context.Background(), server, observed, time.Minute)
	t.Cleanup(b.close)
	a, other := attachTestPeer(t, b), attachTestPeer(t, b)
	initializePeer(t, b, a)
	initializePeer(t, b, other)

	// Seven pending HTTP requests fit the real transport's eight request slots.
	// The second client deliberately reuses the fifth cancelled request's ID.
	upstreamIDs := make([]string, 6)
	for i := range upstreamIDs {
		b.handle(a.peer, toolRequest(10+i, fmt.Sprintf("cancel-%d", i)))
		select {
		case message := <-calls:
			upstreamIDs[i] = string(message.ID())
		case <-time.After(5 * time.Second):
			t.Fatal("pending tool call did not reach HTTP endpoint")
		}
	}
	b.handle(other.peer, toolRequest(14, "unrelated-client"))
	select {
	case <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("other client's call did not reach HTTP endpoint")
	}

	waitWrite := func(requestID string) error {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case result := <-observed.writes:
				if result.requestID == requestID {
					return result.err
				}
			case <-b.ctx.Done():
				t.Fatal("transient HTTP cancellation backpressure stopped the shared backend")
			case <-timer.C:
				t.Fatal("cancellation queue did not continue processing")
			}
		}
	}
	for i := range 4 {
		b.handle(a.peer, wire.Notification("notifications/cancelled", map[string]any{"requestId": 10 + i}))
		if err := waitWrite(upstreamIDs[i]); err != nil {
			t.Fatalf("cancellation %d was rejected before control capacity filled: %v", i, err)
		}
		select {
		case requestID := <-controls:
			if requestID != upstreamIDs[i] {
				t.Fatalf("cancellation reached wrong request: got %s, want %s", requestID, upstreamIDs[i])
			}
		case <-time.After(5 * time.Second):
			t.Fatal("accepted cancellation did not occupy an HTTP control slot")
		}
	}
	b.handle(a.peer, wire.Notification("notifications/cancelled", map[string]any{"requestId": 14}))
	if err := waitWrite(upstreamIDs[4]); !errors.Is(err, upstream.ErrBusy) {
		t.Fatalf("fifth cancellation did not hit real HTTP control backpressure: %v", err)
	}

	// A distinct sixth cancellation is a processing barrier: seeing its write
	// proves the loop handled the fifth result without closing or getting stuck.
	b.handle(a.peer, wire.Notification("notifications/cancelled", map[string]any{"requestId": 15}))
	if err := waitWrite(upstreamIDs[5]); !errors.Is(err, upstream.ErrBusy) {
		t.Fatalf("control slots should remain occupied until released: %v", err)
	}
	unblockControls()
	unblockCalls()
	response := other.read(t)
	if string(response.ID()) != "14" || response["error"] != nil || !strings.Contains(string(response["result"]), "unrelated-client") {
		t.Fatalf("another client's pending call was lost or cancelled: %v", response)
	}
	b.handle(other.peer, toolRequest(20, "after-backpressure"))
	response = other.read(t)
	if string(response.ID()) != "20" || response["error"] != nil || !strings.Contains(string(response["result"]), "after-backpressure") {
		t.Fatalf("shared backend did not accept a new call after backpressure: %v", response)
	}
	for _, p := range []testPeer{a, other} {
		select {
		case <-p.peer.done:
			t.Fatal("cancellation backpressure disconnected a shared client")
		default:
		}
	}
}
