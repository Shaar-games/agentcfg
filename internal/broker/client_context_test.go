package broker

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"agentcfg/internal/ipc"
	"agentcfg/internal/wire"
)

func TestGetStatusHonorsCancellationAfterDial(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	endpoint, err := ipc.ForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Prepare(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(endpoint.Directory) })
	listener, err := ipc.Listen(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connected := make(chan net.Conn, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		connected <- conn
		_, _ = wire.NewReader(conn).Read()
		// A broker that accepts a request but never replies must not freeze a
		// menu or ignore Ctrl+C until the separate 15-second IPC limit expires.
		cancel()
	}()
	result := make(chan error, 1)
	go func() {
		_, err := GetStatus(ctx, root)
		result <- err
	}()
	var conn net.Conn
	select {
	case conn = <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("status did not dial the isolated listener")
	}
	defer conn.Close()
	<-serverDone
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("status error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		_ = conn.Close()
		<-result
		t.Fatal("status ignored cancellation after its IPC connection was established")
	}
}

func TestGetStatusCancelledIsNotReportedAsStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := GetStatus(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled status probe was reported as success: %v", err)
	}
}

type deadlineRecordingConn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
}

func (c *deadlineRecordingConn) SetDeadline(value time.Time) error {
	c.mu.Lock()
	c.deadline = value
	c.mu.Unlock()
	return c.Conn.SetDeadline(value)
}

func TestExchangeReleasesHandshakeDeadlineAndCancellationHook(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	recorded := &deadlineRecordingConn{Conn: client}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = wire.NewReader(server).Read()
		_ = wire.NewWriter(server).Write(wire.Message{"ok": wire.Raw(true)})
	}()
	reader, _, err := exchange(ctx, recorded, hello("connect"))
	if err != nil {
		t.Fatal(err)
	}
	recorded.mu.Lock()
	deadline := recorded.deadline
	recorded.mu.Unlock()
	if !deadline.IsZero() {
		t.Fatalf("handshake left a deadline on the MCP stream: %s", deadline)
	}
	// The completed handshake must detach its cancellation hook. The caller
	// owns the successfully returned stream and its subsequent lifetime.
	cancel()
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() { written <- wire.NewWriter(server).Write(wire.Notification("test", nil)) }()
	message, err := reader.Read()
	if err != nil || message.Method() != "test" {
		t.Fatalf("completed exchange retained a cancellation hook: %v, %v", message, err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

func TestExchangeDeadlineInterruptsBlockedWrite(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, _, err := exchange(ctx, client, hello("status"))
		finished <- err
	}()
	select {
	case err := <-finished:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocked exchange error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		_ = client.Close()
		<-finished
		t.Fatal("blocked IPC write ignored its caller deadline")
	}
}
