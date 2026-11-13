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

func TestSharedInitializationDisconnectReleasesWaitingClients(t *testing.T) {
	transport := &deferredInitializeTransport{newFake()}
	b := newBackend(context.Background(), config.Server{Name: "fixture"}, transport, 10*time.Second)
	t.Cleanup(b.close)
	first, second := attachTestPeer(t, b), attachTestPeer(t, b)
	initialize := wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(1), "method": wire.Raw("initialize"),
		"params": wire.Raw(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})}
	b.handle(first.peer, initialize)
	_ = pendingInitialize(t, transport)
	first.peer.close()
	b.detach(first.peer)
	b.handle(second.peer, initialize)
	request := pendingInitialize(t, transport)
	transport.reads <- wire.Result(request.ID(), map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})
	if response := second.read(t); response["error"] != nil {
		t.Fatalf("second client did not recover: %s", response["error"])
	}
}

func TestSharedInitializationAcceptsCancellationWhileWaiting(t *testing.T) {
	transport := &deferredInitializeTransport{newFake()}
	b := newBackend(context.Background(), config.Server{Name: "fixture"}, transport, 10*time.Second)
	t.Cleanup(b.close)
	p := attachTestPeer(t, b)
	b.handle(p.peer, wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(17), "method": wire.Raw("initialize"),
		"params": wire.Raw(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})})
	request := pendingInitialize(t, transport)
	b.handle(p.peer, wire.Notification("notifications/cancelled", map[string]any{"requestId": 17}))
	if cancellation := nextWrite(t, transport.fakeTransport); cancellation.Method() != "notifications/cancelled" {
		t.Fatalf("initialization cancellation was ignored: %s", cancellation.Method())
	}
	transport.reads <- wire.Error(request.ID(), -32800, "fixture cancelled")
	if response := p.read(t); response["error"] == nil {
		t.Fatal("cancelled initialization unexpectedly succeeded")
	}
}
