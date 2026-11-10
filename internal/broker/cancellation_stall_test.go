package broker

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agentcfg/internal/config"
	"agentcfg/internal/wire"
)

// An upstream can stop reading stdin while its pending operations time out.
// Sending its advisory cancellation must not stop unrelated deadline delivery.
type stalledCancellationTransport struct {
	*fakeTransport
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *stalledCancellationTransport) Write(message wire.Message) error {
	if message.Method() == "notifications/cancelled" {
		f.once.Do(func() { close(f.entered) })
		select {
		case <-f.release:
		case <-f.done:
		}
		return nil
	}
	return f.fakeTransport.Write(message)
}

func TestTimeoutsContinueWhenCancellationWriteBlocks(t *testing.T) {
	f := &stalledCancellationTransport{fakeTransport: newFake(), entered: make(chan struct{}), release: make(chan struct{})}
	b := newBackend(context.Background(), config.Server{Name: "stall", Sharing: "shared"}, f, 40*time.Millisecond)
	defer b.close()
	p := attachTestPeer(t, b)
	initializePeer(t, b, p)
	b.handle(p.peer, toolRequest(10, "first"))
	_ = nextWrite(t, f.fakeTransport)
	b.handle(p.peer, toolRequest(11, "second"))
	_ = nextWrite(t, f.fakeTransport)
	if response := p.read(t); response["error"] == nil {
		t.Fatal("first operation did not time out")
	}
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("first cancellation was not attempted")
	}
	_ = p.conn.SetReadDeadline(time.Now().Add(time.Second))
	response, err := p.reader.Read()
	if err != nil {
		t.Fatalf("a blocked cancellation disabled subsequent request timeouts: %v", err)
	}
	var failure struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(response["error"], &failure); err != nil || failure.Code != -32001 {
		t.Fatalf("wrong timeout result: %v", response)
	}
}

func TestClientCancellationDoesNotBlockItsConnection(t *testing.T) {
	f := &stalledCancellationTransport{fakeTransport: newFake(), entered: make(chan struct{}), release: make(chan struct{})}
	b := newBackend(context.Background(), config.Server{Name: "stall", Sharing: "shared"}, f, time.Hour)
	defer b.close()
	p := attachTestPeer(t, b)
	initializePeer(t, b, p)
	b.handle(p.peer, toolRequest(20, "first"))
	_ = nextWrite(t, f.fakeTransport)
	handled := make(chan struct{})
	go func() {
		b.handle(p.peer, wire.Notification("notifications/cancelled", map[string]any{"requestId": 20}))
		close(handled)
	}()
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("client cancellation blocked its request-processing loop")
	}
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("client cancellation was not attempted")
	}
	// Ping is served locally. A subsequent upstream write can still wait for
	// that server's stdin, but the connection must keep processing local work.
	b.handle(p.peer, wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(21), "method": wire.Raw("ping")})
	if response := p.read(t); string(response.ID()) != "21" || response["error"] != nil {
		t.Fatalf("request after cancellation was not routed: %v", response)
	}
}

func TestClientCancellationBoundsAdvisoryPayload(t *testing.T) {
	b, f := setupBackend(t, false, time.Hour)
	p := attachTestPeer(t, b)
	initializePeer(t, b, p)
	b.handle(p.peer, toolRequest(40, "pending"))
	request := nextWrite(t, f)
	b.handle(p.peer, wire.Notification("notifications/cancelled", map[string]any{
		"requestId": 40, "reason": strings.Repeat("large reason é ", 8192),
		"unrecognized": strings.Repeat("x", 128*1024),
	}))
	cancellation := nextWrite(t, f)
	encoded, err := json.Marshal(cancellation)
	if err != nil || len(encoded) > 8*1024 {
		t.Fatalf("advisory cancellation retained an excessive payload: bytes=%d err=%v", len(encoded), err)
	}
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    string          `json:"reason"`
	}
	if err := json.Unmarshal(cancellation["params"], &params); err != nil || string(params.RequestID) != string(request.ID()) || params.Reason == "" {
		t.Fatalf("bounded cancellation lost its request or reason: %v %v", cancellation, err)
	}
}

func TestDisconnectedClientDoesNotWaitForCancellationWrite(t *testing.T) {
	f := &stalledCancellationTransport{fakeTransport: newFake(), entered: make(chan struct{}), release: make(chan struct{})}
	b := newBackend(context.Background(), config.Server{Name: "stall", Sharing: "shared"}, f, time.Hour)
	defer b.close()
	p := attachTestPeer(t, b)
	initializePeer(t, b, p)
	b.handle(p.peer, toolRequest(30, "pending"))
	_ = nextWrite(t, f.fakeTransport)
	detached := make(chan struct{})
	go func() { b.detach(p.peer); close(detached) }()
	select {
	case <-detached:
	case <-time.After(time.Second):
		t.Fatal("a blocked upstream prevented client cleanup")
	}
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("disconnected request was not cancelled")
	}
	b.mu.Lock()
	remaining := len(b.pending)
	b.mu.Unlock()
	if remaining != 0 {
		t.Fatal("disconnected client's pending operations were retained")
	}
}

func TestCancellationBackpressureStopsUnresponsiveBackend(t *testing.T) {
	f := &stalledCancellationTransport{fakeTransport: newFake(), entered: make(chan struct{}), release: make(chan struct{})}
	b := newBackend(context.Background(), config.Server{Name: "stall", Sharing: "shared"}, f, time.Hour)
	defer b.close()
	message := wire.Notification("notifications/cancelled", map[string]any{"requestId": 1})
	b.queueCancellation(message)
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("cancellation was not attempted")
	}
	for range maxPendingRequests + 1 {
		b.queueCancellation(message)
	}
	select {
	case <-b.ctx.Done():
	default:
		t.Fatal("an upstream that cannot drain cancellations was kept alive")
	}
	select {
	case <-f.done:
	default:
		t.Fatal("stalled write was not interrupted by closing its transport")
	}
}
