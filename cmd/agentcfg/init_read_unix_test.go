//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestNativeSkillImportOpensUnexpectedFIFOWithoutBlocking(t *testing.T) {
	// Model a regular asset that was replaced by a FIFO between Lstat and
	// OpenFile. The importer must be able to inspect and reject the opened
	// object even when no process has opened the write side of the FIFO.
	home := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(home, "asset"), 0o600); err != nil {
		t.Skipf("cannot create FIFO on this filesystem: %v", err)
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, err := root.OpenFile("asset", os.O_RDONLY|initNonblockRead, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().IsRegular() {
		t.Fatal("FIFO unexpectedly passed regular-file check")
	}
}
