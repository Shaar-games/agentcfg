package broker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentmux/internal/config"
	"agentmux/internal/wire"
)

type fakeTransport struct {
	writes          chan wire.Message
	reads           chan wire.Message
	done            chan struct{}
	once            sync.Once
	initializes     atomic.Int32
	discovers       atomic.Int32
	initializeInput chan wire.Message
}

func newFake() *fakeTransport {
	return &fakeTransport{writes: make(chan wire.Message, 64), reads: make(chan wire.Message, 64), done: make(chan struct{}), initializeInput: make(chan wire.Message, 8)}
}

func (f *fakeTransport) Read() (wire.Message, error) {
	select {
	case message := <-f.reads:
		return message, nil
	case <-f.done:
		return nil, io.EOF
	}
}

func (f *fakeTransport) Write(message wire.Message) error {
	if message.Method() == "server/discover" {
		f.discovers.Add(1)
		f.reads <- wire.Result(message.ID(), map[string]any{"supportedVersions": []string{wire.ModernProtocolVersion}, "capabilities": map[string]any{"tools": map[string]any{}}})
		return nil
	}
	if message.Method() == "initialize" {
		f.initializes.Add(1)
		f.initializeInput <- message.Clone()
		f.reads <- wire.Result(message.ID(), map[string]any{"protocolVersion": "2025-11-25", "serverInfo": map[string]string{"name": "fake", "version": "1"}, "capabilities": map[string]any{"tools": map[string]any{}, "logging": map[string]any{}, "resources": map[string]any{"subscribe": true}}})
		return nil
	}
	if message.Method() == "notifications/initialized" {
		return nil
	}
	select {
	case f.writes <- message.Clone():
		return nil
	case <-f.done:
		return io.ErrClosedPipe
	}
}

func (f *fakeTransport) Close() error { f.once.Do(func() { close(f.done) }); return nil }
func (f *fakeTransport) PID() int     { return 12345 }

type testPeer struct {
	peer   *peer
	conn   net.Conn
	reader *wire.Reader
}

func setupBackend(t *testing.T, isolated bool, timeout time.Duration) (*backend, *fakeTransport) {
	t.Helper()
	f := newFake()
	sharing := "shared"
	if isolated {
		sharing = "isolated"
	}
	b := newBackend(context.Background(), config.Server{Name: "test", Sharing: sharing}, f, timeout)
	t.Cleanup(b.close)
	return b, f
}

func attachTestPeer(t *testing.T, b *backend) testPeer {
	t.Helper()
	server, client := net.Pipe()
	p := newPeer(server)
	go p.writeLoop()
	if err := b.attach(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.close(); b.detach(p); client.Close() })
	return testPeer{peer: p, conn: client, reader: wire.NewReader(client)}
}

func (p testPeer) read(t *testing.T) wire.Message {
	t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	message, err := p.reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func initializePeer(t *testing.T, b *backend, p testPeer) {
	t.Helper()
	b.handle(p.peer, wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(1), "method": wire.Raw("initialize"), "params": wire.Raw(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"roots": map[string]any{}, "sampling": map[string]any{}}, "clientInfo": map[string]string{"name": "harness", "version": "1"}})})
	response := p.read(t)
	if response["error"] != nil {
		t.Fatalf("initialize: %s", response["error"])
	}
}

func nextWrite(t *testing.T, f *fakeTransport) wire.Message {
	t.Helper()
	select {
	case m := <-f.writes:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("upstream write not received")
		return nil
	}
}

func toolRequest(id any, name string) wire.Message {
	return wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(id), "method": wire.Raw("tools/call"), "params": wire.Raw(map[string]any{"name": name, "_meta": map[string]any{"progressToken": "same-token"}})}
}

func TestOneInitializeForTwoClients(t *testing.T) {
	b, f := setupBackend(t, false, time.Second)
	a, c := attachTestPeer(t, b), attachTestPeer(t, b)
	initializePeer(t, b, a)
	initializePeer(t, b, c)
	if f.initializes.Load() != 1 {
		t.Fatalf("initialized %d times", f.initializes.Load())
	}
	input := <-f.initializeInput
	var params map[string]json.RawMessage
	if err := json.Unmarshal(input["params"], &params); err != nil {
		t.Fatal(err)
	}
	if string(params["capabilities"]) != "{}" {
		t.Fatal("advertised ambiguous client callbacks", string(params["capabilities"]))
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(b.initResult, &result); err != nil {
		t.Fatal(err)
	}
	var caps map[string]json.RawMessage
	_ = json.Unmarshal(result["capabilities"], &caps)
	if caps["logging"] != nil {
		t.Fatal("advertised session-global logging")
	}
}

func TestCollidingIDsAndProgressAreRoutedToTheirOwner(t *testing.T) {
	b, f := setupBackend(t, false, time.Second)
	a, c := attachTestPeer(t, b), attachTestPeer(t, b)
	initializePeer(t, b, a)
	initializePeer(t, b, c)
	b.handle(a.peer, toolRequest(7, "first"))
	first := nextWrite(t, f)
	b.handle(c.peer, toolRequest(7, "second"))
	second := nextWrite(t, f)
	if string(first.ID()) == string(second.ID()) {
		t.Fatal("upstream IDs collide")
	}
	var params map[string]any
	_ = json.Unmarshal(second["params"], &params)
	token := params["_meta"].(map[string]any)["progressToken"]
	if token == "same-token" {
		t.Fatal("progress token was not remapped")
	}
	f.reads <- wire.Notification("notifications/progress", map[string]any{"progressToken": token, "progress": 1})
	f.reads <- wire.Result(second.ID(), map[string]string{"owner": "second"})
	progress := c.read(t)
	var restored map[string]any
	_ = json.Unmarshal(progress["params"], &restored)
	if progress.Method() != "notifications/progress" || restored["progressToken"] != "same-token" {
		t.Fatal(progress)
	}
	response := c.read(t)
	if string(response.ID()) != "7" || string(response["result"]) != `{"owner":"second"}` {
		t.Fatal(response)
	}
	f.reads <- wire.Result(first.ID(), map[string]string{"owner": "first"})
	response = a.read(t)
	if string(response.ID()) != "7" || string(response["result"]) != `{"owner":"first"}` {
		t.Fatal(response)
	}
}

func TestCancellationDoesNotCancelOtherClientSameID(t *testing.T) {
	b, f := setupBackend(t, false, time.Second)
	a, c := attachTestPeer(t, b), attachTestPeer(t, b)
	initializePeer(t, b, a)
	initializePeer(t, b, c)
	b.handle(a.peer, toolRequest("shared-id", "first"))
	first := nextWrite(t, f)
	b.handle(c.peer, toolRequest("shared-id", "second"))
	second := nextWrite(t, f)
	b.handle(a.peer, wire.Notification("notifications/cancelled", map[string]any{"requestId": "shared-id", "reason": "user cancelled"}))
	cancel := nextWrite(t, f)
	var params map[string]json.RawMessage
	_ = json.Unmarshal(cancel["params"], &params)
	if string(params["requestId"]) != string(first.ID()) || string(params["requestId"]) == string(second.ID()) {
		t.Fatal(cancel)
	}
}

func TestTimeoutReturnsErrorWithoutRetryingTheOperation(t *testing.T) {
	b, f := setupBackend(t, false, 30*time.Millisecond)
	a := attachTestPeer(t, b)
	initializePeer(t, b, a)
	b.handle(a.peer, toolRequest(4, "slow"))
	_ = nextWrite(t, f)
	response := a.read(t)
	var failure struct {
		Code int `json:"code"`
	}
	_ = json.Unmarshal(response["error"], &failure)
	if failure.Code != -32001 {
		t.Fatal(response)
	}
	if nextWrite(t, f).Method() != "notifications/cancelled" {
		t.Fatal("request was retried instead of cancelled")
	}
}

func TestServerCallbacksRejectedInSharedMode(t *testing.T) {
	b, f := setupBackend(t, false, time.Second)
	a := attachTestPeer(t, b)
	initializePeer(t, b, a)
	f.reads <- wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw("callback"), "method": wire.Raw("sampling/createMessage"), "params": wire.Raw(map[string]any{})}
	response := nextWrite(t, f)
	if response["error"] == nil || string(response.ID()) != `"callback"` {
		t.Fatal(response)
	}
}

func TestIsolatedModePreservesCallbacksAndIDs(t *testing.T) {
	b, f := setupBackend(t, true, time.Second)
	a := attachTestPeer(t, b)
	f.reads <- wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(98), "method": wire.Raw("roots/list"), "params": wire.Raw(map[string]any{})}
	request := a.read(t)
	if request.Method() != "roots/list" || string(request.ID()) != "98" {
		t.Fatal(request)
	}
	b.handle(a.peer, wire.Result(request.ID(), map[string]any{"roots": []any{}}))
	response := nextWrite(t, f)
	if string(response.ID()) != "98" || response["result"] == nil {
		t.Fatal(response)
	}
}

func TestDisconnectedClientDoesNotStopSharedBackend(t *testing.T) {
	b, f := setupBackend(t, false, time.Second)
	a, c := attachTestPeer(t, b), attachTestPeer(t, b)
	initializePeer(t, b, a)
	initializePeer(t, b, c)
	a.peer.close()
	b.detach(a.peer)
	select {
	case <-f.done:
		t.Fatal("shared backend stopped")
	default:
	}
	b.handle(c.peer, toolRequest(12, "survivor"))
	request := nextWrite(t, f)
	f.reads <- wire.Result(request.ID(), map[string]bool{"alive": true})
	if response := c.read(t); string(response["result"]) != `{"alive":true}` {
		t.Fatal(response)
	}
}

func TestUpstreamCrashClosesClientsWithoutReplay(t *testing.T) {
	b, f := setupBackend(t, false, time.Second)
	a := attachTestPeer(t, b)
	initializePeer(t, b, a)
	b.handle(a.peer, toolRequest(5, "mutating"))
	_ = nextWrite(t, f)
	_ = f.Close()
	_ = a.conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := a.reader.Read(); err == nil {
		t.Fatal("upstream crash did not close client")
	}
	select {
	case m := <-f.writes:
		t.Fatal("unexpected replay", m)
	default:
	}
}

func TestSharedSessionSpecificOperationsAreRejected(t *testing.T) {
	b, f := setupBackend(t, false, time.Second)
	a := attachTestPeer(t, b)
	initializePeer(t, b, a)
	for _, method := range []string{"tasks/get", "tasks/cancel", "resources/subscribe", "logging/setLevel"} {
		b.handle(a.peer, wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(12), "method": wire.Raw(method), "params": wire.Raw(map[string]any{})})
		if response := a.read(t); response["error"] == nil {
			t.Fatalf("session-bound %s accepted", method)
		}
	}
	select {
	case message := <-f.writes:
		t.Fatal("forwarded session-bound operation", message)
	default:
	}
}

func TestEmptyHarnessSelectionDisablesRegisteredHarnesses(t *testing.T) {
	if harnessEnabled(nil, "cursor") {
		t.Fatal("empty selection enabled cursor")
	}
	if !harnessEnabled([]string{"cursor"}, "cursor") {
		t.Fatal("selected harness rejected")
	}
	if harnessEnabled([]string{"codex"}, "cursor") {
		t.Fatal("unselected harness accepted")
	}
}
