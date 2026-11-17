package ipc

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	testPrivateIPCExchange(t, endpoint)
}

func testPrivateIPCExchange(t *testing.T, endpoint Endpoint) {
	t.Helper()
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

func TestSocketAddressPreservesValidLegacyPaths(t *testing.T) {
	for _, limit := range []int{103, 107} {
		for _, extra := range []int{-1, 0, 1, 10, 11} {
			directory := string(filepath.Separator) + strings.Repeat("d", limit+extra-len("/broker.sock")-1)
			address, err := socketAddress(directory, limit)
			if extra > 10 {
				if err == nil || address != "" || !strings.Contains(err.Error(), "TMPDIR") {
					t.Fatalf("limit=%d extra=%d: unusable path accepted: %q %v", limit, extra, address, err)
				}
				continue
			}
			want := "broker.sock"
			if extra > 0 {
				want = "s"
			}
			if err != nil || filepath.Dir(address) != directory || filepath.Base(address) != want || len(address) > limit {
				t.Fatalf("limit=%d extra=%d: wrong private endpoint: %q %v", limit, extra, address, err)
			}
		}
	}
	// Limits count UTF-8 bytes, not visible characters.
	directory := string(filepath.Separator) + strings.Repeat("é", 47)
	address, err := socketAddress(directory, 103)
	if err != nil || filepath.Base(address) != "s" {
		t.Fatalf("Unicode path exceeded its byte budget: %q %v", address, err)
	}
}
