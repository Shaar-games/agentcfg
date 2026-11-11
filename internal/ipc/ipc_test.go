package ipc

import (
	"context"
	"io"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestPrivateIPCExchange(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	endpoint, err := ForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Prepare(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(endpoint.Directory) })
	listener, err := Listen(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_, err = io.CopyN(conn, conn, 4)
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	var received [4]byte
	if _, err := io.ReadFull(conn, received[:]); err != nil {
		t.Fatal(err)
	}
	if string(received[:]) != "ping" {
		t.Fatal(string(received[:]))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(endpoint.Directory)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatal("runtime directory is not private")
		}
		info, err = os.Stat(endpoint.Address)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatal("socket is not private")
		}
	}
}
