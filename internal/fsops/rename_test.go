package fsops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRenameNeverAdoptsExistingDestination(t *testing.T) {
	root := t.TempDir()
	source, dest := filepath.Join(root, "source"), filepath.Join(root, "dest")
	if err := os.WriteFile(source, []byte("staged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("user data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Rename(context.Background(), source, dest, nil); err == nil {
		t.Fatal("existing destination overwritten")
	}
	if data, _ := os.ReadFile(dest); string(data) != "user data" {
		t.Fatal("user data changed")
	}
}

func TestRenameChecksCancellationAfterValidation(t *testing.T) {
	root := t.TempDir()
	source, dest := filepath.Join(root, "source"), filepath.Join(root, "dest")
	if err := os.WriteFile(source, []byte("staged"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := Rename(ctx, source, dest, func() error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatal("cancelled rename wrote its destination")
	}
}
