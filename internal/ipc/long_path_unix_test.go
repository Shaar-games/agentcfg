//go:build !windows

package ipc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateIPCExchangeWithLongTemporaryDirectory(t *testing.T) {
	root := t.TempDir()
	// This is a second owned temporary directory under the inherited TMPDIR,
	// never a fixed system path or the actual user profile.
	parent, err := os.MkdirTemp("", "ip-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	suffix := string(filepath.Separator) + "agentcfg-" + strings.Repeat("0", 24) + string(filepath.Separator) + "s"
	length := maxUnixSocketPath - len(suffix)
	if len(parent) > length {
		t.Fatalf("test TMPDIR is too long for a Unix socket fixture; use a shorter temporary directory: %d > %d", len(parent), length)
	}
	temporary := parent
	// If only one byte remains, use the parent: a trailing slash is cleaned
	// by Join, and the old socket name still exceeds the OS limit.
	if padding := length - len(parent); padding > 1 {
		temporary = filepath.Join(parent, strings.Repeat("p", padding-1))
	}
	if err := os.MkdirAll(temporary, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", temporary)
	cache := filepath.Join(root, strings.Repeat("c", 110))
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	endpoint, err := ForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(endpoint.Directory) != temporary || filepath.Base(endpoint.Address) != "s" || len(endpoint.Address) > maxUnixSocketPath {
		t.Fatalf("fallback left the owned temporary directory or did not fit: %+v", endpoint)
	}
	testPrivateIPCExchange(t, endpoint)
}

func TestForRootRejectsUnusableTemporaryDirectoryWithoutWriting(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, strings.Repeat("c", 110))
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	temporary := filepath.Join(root, strings.Repeat("t", maxUnixSocketPath))
	t.Setenv("TMPDIR", temporary)
	endpoint, err := ForRoot(root)
	if err == nil || endpoint != (Endpoint{}) || !strings.Contains(err.Error(), "TMPDIR") {
		t.Fatalf("oversized fallback lacks an actionable error: %+v %v", endpoint, err)
	}
	if _, err := os.Stat(temporary); !os.IsNotExist(err) {
		t.Fatalf("endpoint selection wrote outside a usable runtime directory: %v", err)
	}
}
