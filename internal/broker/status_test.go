package broker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/ipc"
	"agentcfg/internal/wire"
)

// A stopped broker can close its listener after the status caller has dialed
// but before it has replied. This must be observed as stopped, not an IPC
// failure. The test uses a local private endpoint, not a running MCP server.
func TestGetStatusDuringIPCShutdown(t *testing.T) {
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
	served := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		_, err = wire.NewReader(connection).Read()
		_ = listener.Close()
		_ = connection.Close()
		served <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, err := GetStatus(ctx, root)
	if err != nil {
		t.Fatalf("status during broker shutdown: %v", err)
	}
	if status.Running || len(status.Backends) != 0 {
		t.Fatalf("shutdown broker reported active: %+v", status)
	}
	if err := <-served; err != nil {
		t.Fatalf("receive status request before closing: %v", err)
	}
}

func TestGetStatusPreservesProtocolErrors(t *testing.T) {
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
	served := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		defer connection.Close()
		_, err = wire.NewReader(connection).Read()
		if err == nil {
			err = wire.NewWriter(connection).Write(wire.Message{"error": wire.Raw("deliberate status denial")})
		}
		served <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = GetStatus(ctx, root)
	if err == nil || !strings.Contains(err.Error(), "deliberate status denial") {
		t.Fatalf("real server rejection was hidden or changed: %v", err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}
