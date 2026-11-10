//go:build unix

package syncer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestInitRestorePointRejectsFIFOWithoutBlocking(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agentcfg", initRestoreFile)
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := readInitPoint(home)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("initialization restore point accepted a FIFO")
		}
	case <-time.After(time.Second):
		// A regression must not leave its blocked read running after failure.
		writer, err := os.OpenFile(path, os.O_WRONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			t.Fatalf("restore point read blocked on FIFO; unblock: %v", err)
		}
		_, _ = writer.Write([]byte("{}\n"))
		_ = writer.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("restore point read kept blocking after releasing FIFO")
		}
		t.Fatal("initialization restore point blocked while reading a FIFO")
	}
}
