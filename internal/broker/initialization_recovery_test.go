package broker

import (
	"context"
	"testing"
	"time"

	"agentcfg/internal/config"
	"agentcfg/internal/wire"
)

func TestSharedInitializationCanRetryInvalidParams(t *testing.T) {
	b, transport := setupBackend(t, false, time.Second)
	p := attachTestPeer(t, b)
	b.handle(p.peer, wire.Message{
		"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(1),
		"method": wire.Raw("initialize"), "params": wire.Raw(map[string]any{}),
	})
	if response := p.read(t); response["error"] == nil {
		t.Fatalf("invalid initialize accepted: %v", response)
	}
	// A failed local validation did not initialize the upstream. A corrected
	// handshake on this connection must remain possible, just as after an
	// upstream JSON-RPC initialization error or failed modern discovery.
	initializePeer(t, b, p)
	if got := transport.initializes.Load(); got != 1 {
		t.Fatalf("upstream initialized %d times, want one corrected handshake", got)
	}
}

type deferredInitializeTransport struct{ *fakeTransport }

func (f *deferredInitializeTransport) Write(message wire.Message) error {
	if message.Method() == "initialize" {
		f.initializes.Add(1)
		f.initializeInput <- message.Clone()
		return nil
	}
	return f.fakeTransport.Write(message)
}

func pendingInitialize(t *testing.T, transport *deferredInitializeTransport) wire.Message {
	t.Helper()
	select {
	case request := <-transport.initializeInput:
		return request
	case <-time.After(3 * time.Second):
		t.Fatal("initialization remained blocked by the previous client")
		return nil
	}
}

func assertInitializationStillPending(t *testing.T, b *backend, request wire.Message) {
	t.Helper()
	b.mu.Lock()
	pending := b.pending[string(request.ID())] != nil
	b.mu.Unlock()
	if !pending {
		t.Fatal("disconnect discarded an upstream initialize that has not returned a result")
	}
}

func assertNoInitializationCancellation(t *testing.T, b *backend, transport *deferredInitializeTransport) {
	t.Helper()
	// A marker queued after the client's action drains every preceding advisory
	// write. This checks absence of cancellation without depending on a sleep or
	// on whether the cancellation goroutine has already been scheduled.
	const marker = "fixture/cancellation-barrier"
	b.queueCancellation(wire.Notification(marker, nil))
	if message := nextWrite(t, transport.fakeTransport); message.Method() != marker {
		t.Fatalf("unexpected upstream message during initialization: %s", message.Method())
	}
}

func TestSharedInitializationDisconnectKeepsSuccessfulHandshake(t *testing.T) {
	transport := &deferredInitializeTransport{newFake()}
	b := newBackend(context.Background(), config.Server{Name: "fixture"}, transport, 10*time.Second)
	t.Cleanup(b.close)
	first, second := attachTestPeer(t, b), attachTestPeer(t, b)
	initialize := wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(1), "method": wire.Raw("initialize"),
		"params": wire.Raw(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})}
	b.handle(first.peer, initialize)
	request := pendingInitialize(t, transport)
	first.peer.close()
	b.detach(first.peer)
	assertInitializationStillPending(t, b, request)
	assertNoInitializationCancellation(t, b, transport)
	b.handle(second.peer, initialize)
	transport.reads <- wire.Result(request.ID(), map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})
	if response := second.read(t); response["error"] != nil {
		t.Fatalf("second client did not reuse the shared handshake: %s", response["error"])
	}
	if got := transport.initializes.Load(); got != 1 {
		t.Fatalf("upstream initialized %d times, want one shared handshake", got)
	}
}

func TestSharedInitializationDisconnectWaitsForFailureBeforeRetry(t *testing.T) {
	transport := &deferredInitializeTransport{newFake()}
	b := newBackend(context.Background(), config.Server{Name: "fixture"}, transport, 10*time.Second)
	t.Cleanup(b.close)
	first, second := attachTestPeer(t, b), attachTestPeer(t, b)
	initialize := wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(1), "method": wire.Raw("initialize"),
		"params": wire.Raw(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})}
	b.handle(first.peer, initialize)
	request := pendingInitialize(t, transport)
	first.peer.close()
	b.detach(first.peer)
	assertInitializationStillPending(t, b, request)
	assertNoInitializationCancellation(t, b, transport)
	b.handle(second.peer, initialize)

	// The first HTTP exchange may still be unwinding after its browser callback
	// stops. Only its actual failure makes the upstream ready for a new handshake.
	transport.reads <- wire.Error(request.ID(), -32000, "fixture authorization ended")
	retry := pendingInitialize(t, transport)
	if string(retry.ID()) == string(request.ID()) {
		t.Fatal("retry reused the failed upstream request ID")
	}
	transport.reads <- wire.Result(retry.ID(), map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})
	if response := second.read(t); response["error"] != nil {
		t.Fatalf("second client did not recover after the actual initialization failure: %s", response["error"])
	}
	if got := transport.initializes.Load(); got != 2 {
		t.Fatalf("upstream initialized %d times, want the failed handshake and one retry", got)
	}
}

func TestSharedInitializationIgnoresCancellationWhileWaiting(t *testing.T) {
	transport := &deferredInitializeTransport{newFake()}
	b := newBackend(context.Background(), config.Server{Name: "fixture"}, transport, 10*time.Second)
	t.Cleanup(b.close)
	p := attachTestPeer(t, b)
	b.handle(p.peer, wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(17), "method": wire.Raw("initialize"),
		"params": wire.Raw(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})})
	request := pendingInitialize(t, transport)
	b.handle(p.peer, wire.Notification("notifications/cancelled", map[string]any{"requestId": 17}))
	assertNoInitializationCancellation(t, b, transport)
	transport.reads <- wire.Result(request.ID(), map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})
	if response := p.read(t); response["error"] != nil {
		t.Fatalf("invalid initialization cancellation prevented the handshake: %s", response["error"])
	}
}

func TestSharedInitializationTimeoutClosesUnfinishedTransport(t *testing.T) {
	transport := &deferredInitializeTransport{newFake()}
	b := newBackend(context.Background(), config.Server{Name: "fixture"}, transport, 10*time.Second)
	t.Cleanup(b.close)
	p := attachTestPeer(t, b)
	b.handle(p.peer, wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(17), "method": wire.Raw("initialize"),
		"params": wire.Raw(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})})
	_ = pendingInitialize(t, transport)
	b.expire(time.Now().Add(11 * time.Second))
	select {
	case <-b.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("expired initialization left the unfinished backend available for reuse")
	}
	select {
	case <-transport.done:
	case <-time.After(3 * time.Second):
		t.Fatal("expired initialization did not close its upstream transport")
	}
	select {
	case message := <-transport.writes:
		t.Fatalf("expired initialization sent an advisory notification instead of closing: %s", message.Method())
	default:
	}
}
