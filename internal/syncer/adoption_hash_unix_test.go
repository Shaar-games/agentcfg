//go:build unix

package syncer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestAdoptionHashTreeRejectsFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	})
	for _, test := range []struct {
		name string
		hash func(string) (string, error)
	}{
		{"content hash", SkillTreeSHA256},
		{"ownership fingerprint", adoptionTreeFingerprint},
	} {
		t.Run(test.name, func(t *testing.T) {
			type result struct {
				value string
				err   error
			}
			done := make(chan result, 1)
			go func() {
				value, err := test.hash(root)
				done <- result{value, err}
			}()
			select {
			case got := <-done:
				if got.err == nil || got.value != "" {
					t.Fatalf("FIFO produced fingerprint %q, error %v", got.value, got.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("hashing a FIFO blocked instead of rejecting a special file")
			}
		})
	}
}
