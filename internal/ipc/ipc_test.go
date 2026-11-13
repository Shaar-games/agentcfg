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
	checkPrivateIPCExchange(t, endpoint)
}

func TestPrivateIPCLongTemporaryPath(t *testing.T) {
	endpoint := longTemporaryEndpoint(t)
	checkPrivateIPCExchange(t, endpoint)
	if len(endpoint.Address) > 100 {
		t.Fatalf("Unix IPC address exceeds the portable limit: %d bytes", len(endpoint.Address))
	}
}

func TestPrivateIPCFallbackRejectsSymlink(t *testing.T) {
	endpoint := longTemporaryEndpoint(t)
	target := t.TempDir()
	if err := os.Chmod(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(endpoint.Directory); !os.IsNotExist(err) {
		t.Fatalf("test endpoint already exists or cannot be inspected: %v", err)
	}
	if err := os.Symlink(target, endpoint.Directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(endpoint.Directory); err != nil {
			t.Error(err)
		}
	})
	if err := endpoint.Prepare(); err == nil {
		t.Fatal("private IPC directory followed a preexisting symlink")
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("symlink target permissions changed: %v %v", info, err)
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink target was modified: %v %v", entries, err)
	}
}

func longTemporaryEndpoint(t *testing.T) Endpoint {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket path limits do not apply to named pipes")
	}
	base := t.TempDir()
	root := filepath.Join(base, "home")
	cache := filepath.Join(base, strings.Repeat("c", 90))
	temporary := filepath.Join(base, strings.Repeat("t", 90))
	for _, path := range []string{root, cache, temporary} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("LOCALAPPDATA", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("TMPDIR", temporary)
	t.Setenv("TMP", temporary)
	t.Setenv("TEMP", temporary)
	endpoint, err := ForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := ForRoot(root)
	if err != nil || repeated != endpoint {
		t.Fatalf("endpoint changed for the same root: %+v %+v %v", endpoint, repeated, err)
	}
	otherRoot := filepath.Join(base, "other-home")
	if err := os.MkdirAll(otherRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	other, err := ForRoot(otherRoot)
	if err != nil || other == endpoint {
		t.Fatalf("distinct roots shared an endpoint: %+v %v", other, err)
	}
	return endpoint
}

func checkPrivateIPCExchange(t *testing.T, endpoint Endpoint) {
	t.Helper()
	// A fallback may be outside t.TempDir, but its hash belongs exclusively to
	// this test's fresh root. Never remove a directory that already existed.
	if _, err := os.Lstat(endpoint.Directory); !os.IsNotExist(err) {
		t.Fatalf("test endpoint already exists or cannot be inspected: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(endpoint.Directory); err != nil {
			t.Error(err)
		}
	})
	if err := endpoint.Prepare(); err != nil {
		t.Fatal(err)
	}
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
