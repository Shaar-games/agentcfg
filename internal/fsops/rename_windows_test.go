//go:build windows

package fsops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func lockedStage(t *testing.T, directory bool) (string, string, *os.File) {
	t.Helper()
	root := t.TempDir()
	stage := filepath.Join(root, ".agentcfg-layout-staged")
	dest := filepath.Join(root, "twg-code-review")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(stage, "SKILL.md")
	if err := os.WriteFile(asset, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	heldPath := asset
	if directory {
		heldPath = stage
	}
	held, err := os.Open(heldPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Close() })
	return stage, dest, held
}

func TestWindowsRenameWaitsForRealNativeHandles(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "open-skill-file", true: "open-skill-directory"}[directory], func(t *testing.T) {
			stage, dest, held := lockedStage(t, directory)
			originalErr := os.Rename(stage, dest)
			if !errors.Is(originalErr, windows.ERROR_ACCESS_DENIED) && !errors.Is(originalErr, windows.ERROR_SHARING_VIOLATION) {
				t.Fatalf("failed to reproduce native rename denial: %v", originalErr)
			}
			t.Logf("Reproduced: %v", originalErr)
			done := make(chan struct{})
			go func() { defer close(done); time.Sleep(120 * time.Millisecond); held.Close() }()
			checks := 0
			err := Rename(context.Background(), stage, dest, func() error { checks++; return nil })
			<-done
			if err != nil {
				t.Fatal(err)
			}
			if checks < 2 {
				t.Fatal("blocked rename was not revalidated")
			}
			if data, err := os.ReadFile(filepath.Join(dest, "SKILL.md")); err != nil || string(data) != "fixture" {
				t.Fatalf("staged contents lost: %v", err)
			}
		})
	}
}

func TestWindowsRenamePersistentLockIsBounded(t *testing.T) {
	stage, dest, _ := lockedStage(t, false)
	started := time.Now()
	err := rename(context.Background(), stage, dest, false, nil, 60*time.Millisecond)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || !strings.Contains(err.Error(), "still blocked") {
		t.Fatalf("persistent lock not reported: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("retry was not bounded")
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatal("blocked rename changed the destination")
	}
}

func TestWindowsRenameCancellationAndForeignDestination(t *testing.T) {
	for _, kind := range []string{"cancel", "foreign-destination"} {
		t.Run(kind, func(t *testing.T) {
			stage, dest, held := lockedStage(t, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				time.Sleep(70 * time.Millisecond)
				if kind == "cancel" {
					cancel()
					done <- nil
				} else {
					done <- os.WriteFile(dest, []byte("user data"), 0600)
					held.Close()
				}
			}()
			err := Rename(ctx, stage, dest, nil)
			held.Close()
			if workerErr := <-done; workerErr != nil {
				t.Fatal(workerErr)
			}
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if err == nil {
				t.Fatal("blocked transaction unexpectedly committed")
			}
			if kind == "foreign-destination" {
				if data, _ := os.ReadFile(dest); string(data) != "user data" {
					t.Fatal("new destination was overwritten")
				}
			}
		})
	}
}

func TestWindowsAtomicReplaceWaitsForReaderAndPreservesPreimage(t *testing.T) {
	root := t.TempDir()
	source, dest := filepath.Join(root, "stage.json"), filepath.Join(root, "mcp.json")
	if err := os.WriteFile(source, []byte("new configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	done := make(chan struct{})
	go func() { defer close(done); time.Sleep(100 * time.Millisecond); held.Close() }()
	err = Replace(context.Background(), source, dest, func() error {
		data, err := os.ReadFile(dest)
		if err != nil {
			return err
		}
		if string(data) != "original" {
			return errors.New("preimage changed")
		}
		return nil
	})
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "new configuration" {
		t.Fatal("replacement not installed")
	}
}
