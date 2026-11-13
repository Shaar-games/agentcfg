package broker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"agentcfg/internal/config"
	"agentcfg/internal/wire"
)

// Model the HTTP transport's discovery state until cancellation has finished
// draining its POST. An advisory Write can be queued independently of the
// goroutine that must hold the shared initialization lock during that drain.
type drainingDiscoveryTransport struct {
	*fakeTransport
	mu                  sync.Mutex
	active              bool
	requestID           json.RawMessage
	refuseDiscovery     bool
	discoveryStarted    chan wire.Message
	drainStarted        chan struct{}
	drainOnce           sync.Once
	releaseDrain        chan struct{}
	prematureInitialize chan struct{}
	prematureOnce       sync.Once
}

func newDrainingDiscoveryTransport() *drainingDiscoveryTransport {
	return &drainingDiscoveryTransport{
		fakeTransport: newFake(), discoveryStarted: make(chan wire.Message, 1),
		drainStarted: make(chan struct{}), releaseDrain: make(chan struct{}),
		prematureInitialize: make(chan struct{}),
	}
}

func (f *drainingDiscoveryTransport) Write(message wire.Message) error {
	switch message.Method() {
	case "server/discover":
		if f.refuseDiscovery {
			return errors.New("fixture discovery rejected before starting")
		}
		f.mu.Lock()
		f.active = true
		f.requestID = append(json.RawMessage(nil), message.ID()...)
		f.mu.Unlock()
		f.discovers.Add(1)
		f.discoveryStarted <- message.Clone()
		return nil
	case "notifications/cancelled":
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(message["params"], &params) == nil {
			f.drain(params.RequestID, false)
		}
		return nil
	case "initialize":
		f.mu.Lock()
		active := f.active
		f.mu.Unlock()
		if active {
			f.prematureOnce.Do(func() { close(f.prematureInitialize) })
			return errors.New("legacy initialize reached a discovery that has not drained")
		}
	}
	return f.fakeTransport.Write(message)
}

func (f *drainingDiscoveryTransport) CancelRequest(id json.RawMessage) {
	f.drain(id, true)
}

func (f *drainingDiscoveryTransport) drain(id json.RawMessage, direct bool) {
	f.mu.Lock()
	active := f.active && string(f.requestID) == string(id)
	f.mu.Unlock()
	if !active {
		return
	}
	if direct {
		f.drainOnce.Do(func() { close(f.drainStarted) })
	}
	select {
	case <-f.releaseDrain:
	case <-f.done:
	}
	f.mu.Lock()
	if string(f.requestID) == string(id) {
		f.active = false
	}
	f.mu.Unlock()
}

func waitDiscoveryRequest(t *testing.T, transport *drainingDiscoveryTransport) wire.Message {
	t.Helper()
	select {
	case request := <-transport.discoveryStarted:
		return request
	case <-time.After(3 * time.Second):
		t.Fatal("discovery did not reach its upstream transport")
		return nil
	}
}

func TestSharedDiscoveryDrainsBeforeLegacyFallback(t *testing.T) {
	for _, cause := range []string{"disconnect", "expiry"} {
		t.Run(cause, func(t *testing.T) {
			transport := newDrainingDiscoveryTransport()
			b := newBackend(context.Background(), config.Server{Name: "fixture"}, transport, time.Minute)
			t.Cleanup(b.close)
			first, second := attachTestPeer(t, b), attachTestPeer(t, b)
			b.handle(first.peer, modernMessage("probe", "server/discover", nil))
			_ = waitDiscoveryRequest(t, transport)

			returned := make(chan struct{})
			go func() {
				if cause == "disconnect" {
					first.peer.close()
					b.detach(first.peer)
				} else {
					b.expire(time.Now().Add(2 * time.Minute))
				}
				close(returned)
			}()
			select {
			case <-returned:
			case <-time.After(3 * time.Second):
				t.Fatal("peer cleanup or deadline delivery blocked on the discovery drain")
			}
			b.handle(second.peer, wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw("legacy"), "method": wire.Raw("initialize"),
				"params": wire.Raw(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})})
			select {
			case <-transport.prematureInitialize:
				t.Fatal("legacy initialize overtook cancellation of the previous discovery")
			case <-transport.drainStarted:
			case <-time.After(3 * time.Second):
				t.Fatal("discovery did not drain before releasing the shared initialization lock")
			}
			if b.initMu.TryLock() {
				b.initMu.Unlock()
				t.Fatal("shared initialization lock was released before the discovery drain finished")
			}

			// Holding the handshake lock must not hold the peer's IPC read loop.
			b.handle(second.peer, wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw("ping"), "method": wire.Raw("ping")})
			if response := second.read(t); string(response.ID()) != `"ping"` || response["error"] != nil {
				t.Fatalf("peer stopped processing requests while discovery drained: %#v", response)
			}
			close(transport.releaseDrain)
			if response := second.read(t); string(response.ID()) != `"legacy"` || response["error"] != nil {
				t.Fatalf("legacy initialize did not recover after discovery drained: %#v", response)
			}
			if got := transport.initializes.Load(); got != 1 {
				t.Fatalf("legacy initialize count = %d, want one after the drain", got)
			}
		})
	}
}

func TestSharedDiscoveryCompletedOrRejectedAllowsLegacyFallback(t *testing.T) {
	for _, cause := range []string{"response-error", "write-error"} {
		t.Run(cause, func(t *testing.T) {
			transport := newDrainingDiscoveryTransport()
			transport.refuseDiscovery = cause == "write-error"
			b := newBackend(context.Background(), config.Server{Name: "fixture"}, transport, time.Minute)
			t.Cleanup(b.close)
			first, second := attachTestPeer(t, b), attachTestPeer(t, b)
			b.handle(first.peer, modernMessage("probe", "server/discover", nil))
			if cause == "response-error" {
				request := waitDiscoveryRequest(t, transport)
				// HTTP removes its pending request before publishing a real result.
				transport.mu.Lock()
				transport.active = false
				transport.mu.Unlock()
				transport.reads <- wire.Error(request.ID(), -32601, "fixture discovery unsupported")
			}
			if response := first.read(t); string(response.ID()) != `"probe"` || response["error"] == nil {
				t.Fatalf("discovery failure was not returned: %#v", response)
			}
			initializePeer(t, b, second)
			select {
			case <-transport.drainStarted:
				t.Fatal("completed or rejected discovery waited for a nonexistent HTTP request")
			default:
			}
		})
	}
}
