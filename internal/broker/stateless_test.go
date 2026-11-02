package broker

import (
	"agentmux/internal/wire"
	"encoding/json"
	"net"
	"testing"
	"time"
)

func modernMessage(id any, method string, params map[string]any) wire.Message {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    wire.ModernProtocolVersion,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		"io.modelcontextprotocol/clientInfo":         map[string]string{"name": "harness", "version": "1"},
	}
	return wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(id), "method": wire.Raw(method), "params": wire.Raw(params)}
}

func TestModernSharedMCPAcrossClientsWithoutInitialize(t *testing.T) {
	b, upstream := setupBackend(t, false, time.Second)
	a, c := attachTestPeer(t, b), attachTestPeer(t, b)
	b.handle(a.peer, modernMessage(1, "server/discover", nil))
	b.handle(c.peer, modernMessage(1, "server/discover", nil))
	for _, p := range []testPeer{a, c} {
		answer := p.read(t)
		if answer["error"] != nil || string(answer.ID()) != "1" || p.peer.initialized.Load() != 2 {
			t.Fatalf("modern discovery not routed: %v", answer)
		}
	}
	if upstream.initializes.Load() != 0 || upstream.discovers.Load() != 2 {
		t.Fatalf("modern server initialized legacy session: init=%d discover=%d", upstream.initializes.Load(), upstream.discovers.Load())
	}
	b.handle(a.peer, modernMessage(55, "tools/call", map[string]any{"name": "from-a"}))
	b.handle(c.peer, modernMessage(55, "tools/call", map[string]any{"name": "from-c"}))
	first, second := nextWrite(t, upstream), nextWrite(t, upstream)
	if string(first.ID()) == string(second.ID()) {
		t.Fatal("colliding modern RPC IDs")
	}
	var payload map[string]any
	if err := json.Unmarshal(first["params"], &payload); err != nil {
		t.Fatal(err)
	}
	if payload["_meta"].(map[string]any)["io.modelcontextprotocol/protocolVersion"] != wire.ModernProtocolVersion {
		t.Fatal("lost per-request metadata", payload)
	}
	upstream.reads <- wire.Result(second.ID(), map[string]string{"owner": "second"})
	upstream.reads <- wire.Result(first.ID(), map[string]string{"owner": "first"})
	if got := c.read(t); string(got.ID()) != "55" || string(got["result"]) != `{"owner":"second"}` {
		t.Fatal(got)
	}
	if got := a.read(t); string(got.ID()) != "55" || string(got["result"]) != `{"owner":"first"}` {
		t.Fatal(got)
	}
}

func TestModernDirectRequestWithoutDiscover(t *testing.T) {
	b, upstream := setupBackend(t, false, time.Second)
	p := attachTestPeer(t, b)
	b.handle(p.peer, modernMessage(7, "tools/list", nil))
	request := nextWrite(t, upstream)
	if request.Method() != "tools/list" {
		t.Fatal(request)
	}
	upstream.reads <- wire.Result(request.ID(), map[string]any{"tools": []any{}})
	if response := p.read(t); string(response.ID()) != "7" || response["error"] != nil {
		t.Fatal(response)
	}
	if upstream.initializes.Load() != 0 || p.peer.initialized.Load() != 2 {
		t.Fatal("modern direct call initialized legacy session")
	}
}

func TestMixedProtocolErasDoNotShareOneBackend(t *testing.T) {
	b, _ := setupBackend(t, false, time.Second)
	modern, legacy := attachTestPeer(t, b), attachTestPeer(t, b)
	b.handle(modern.peer, modernMessage(1, "server/discover", nil))
	if response := modern.read(t); response["error"] != nil {
		t.Fatal(response)
	}
	legacyRequest := wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(3), "method": wire.Raw("initialize"),
		"params": wire.Raw(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "legacy", "version": "1"}})}
	b.handle(legacy.peer, legacyRequest)
	if response := legacy.read(t); response["error"] == nil {
		t.Fatal("mixed protocol eras accepted without isolation")
	}
}

func TestModernVersionEnvelopeValidation(t *testing.T) {
	valid := modernMessage(1, "tools/list", nil)
	if !wire.IsModernRequest(valid) {
		t.Fatal("valid modern envelope rejected")
	}
	delete(valid, "params")
	if wire.IsModernRequest(valid) {
		t.Fatal("missing per-request metadata accepted")
	}
}

func TestSharedBackendDoesNotBroadcastClientScopedNotifications(t *testing.T) {
	b, _ := setupBackend(t, false, time.Second)
	serverA, clientA := net.Pipe()
	serverC, clientC := net.Pipe()
	a := newPeer(serverA)
	c := newPeer(serverC)
	a.initialized.Store(2)
	c.initialized.Store(2)
	if err := b.attach(a); err != nil {
		t.Fatal(err)
	}
	if err := b.attach(c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b.detach(a)
		b.detach(c)
		a.close()
		c.close()
		clientA.Close()
		clientC.Close()
	})
	for _, method := range []string{"notifications/subscriptions/acknowledged", "notifications/resources/updated", "notifications/logging/message", "notifications/private-data"} {
		b.route(wire.Notification(method, map[string]any{"subscriptionId": "private", "data": "secret"}))
	}
	for _, p := range []*peer{a, c} {
		select {
		case message := <-p.out:
			t.Fatalf("leaked a request-scoped notification to another harness: %v", message)
		default:
		}
	}
	b.route(wire.Notification("notifications/tools/list_changed", map[string]any{}))
	for _, p := range []*peer{a, c} {
		select {
		case message := <-p.out:
			if message.Method() != "notifications/tools/list_changed" {
				t.Fatal(message)
			}
		default:
			t.Fatal("safe global list invalidation was not broadcast")
		}
	}
}

func TestSharedMCPRejectsSubscriptionListenWithoutIsolation(t *testing.T) {
	b, _ := setupBackend(t, false, time.Second)
	p := attachTestPeer(t, b)
	b.handle(p.peer, modernMessage(1, "server/discover", nil))
	if response := p.read(t); response["error"] != nil {
		t.Fatal(response)
	}
	b.handle(p.peer, modernMessage(3, "subscriptions/listen", nil))
	if response := p.read(t); response["error"] == nil {
		t.Fatal("shared backend silently accepted client-specific subscription")
	}
}

func TestStatelessPOSTNotificationsAreRoutedToOriginatingHarness(t *testing.T) {
	b, upstream := setupBackend(t, false, time.Second)
	a, c := attachTestPeer(t, b), attachTestPeer(t, b)
	a.peer.initialized.Store(2)
	c.peer.initialized.Store(2)
	b.handle(a.peer, modernMessage(42, "tools/call", map[string]any{"name": "owned"}))
	sent := nextWrite(t, upstream)
	b.routeFrom(wire.Notification("notifications/resources/updated", map[string]any{"uri": "private://owner"}), sent.ID())
	event := a.read(t)
	if event.Method() != "notifications/resources/updated" {
		t.Fatalf("request-owned notification not routed: %v", event)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if other, err := c.reader.Read(); err == nil {
		t.Fatalf("request notification leaked to another client: %v", other)
	}
}
