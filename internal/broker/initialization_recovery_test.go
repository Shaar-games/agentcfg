package broker

import (
	"testing"
	"time"

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
