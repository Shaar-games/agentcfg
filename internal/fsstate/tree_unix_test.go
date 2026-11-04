//go:build !windows

package fsstate

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSpecialFilesAndLinkedSpecialFilesAreRejected(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	link(t, fifo, filepath.Join(root, "alias"))
	for _, path := range []string{fifo, filepath.Join(root, "alias")} {
		if _, err := Capture(path, Options{FollowLinks: true}); err == nil {
			t.Fatal("special file accepted by snapshot")
		}
		if _, err := ReadFile(path, "", 1024); err == nil {
			t.Fatal("special file accepted by reader")
		}
		if _, err := Copy(path, filepath.Join(t.TempDir(), "copy"), Options{FollowLinks: true}); err == nil {
			t.Fatal("special file accepted by copy")
		}
	}
	// Model substitution after Lstat: the production open flag must permit
	// inspecting a FIFO even when no writer is connected.
	file, err := os.OpenFile(fifo, os.O_RDONLY|nonblockRead, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || info.Mode().IsRegular() {
		t.Fatalf("unexpected FIFO identity: %v", err)
	}
}
