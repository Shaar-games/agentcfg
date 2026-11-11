//go:build windows

package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/fsops"

	"golang.org/x/sys/windows"
)

func nativeRestoreRetryFixture(t *testing.T, count int) (string, initRestorePoint) {
	t.Helper()
	home := t.TempDir()
	var skills []SkillAdoption
	for index := 0; index < count; index++ {
		name := fmt.Sprintf("review-%d", index)
		native := filepath.Join(home, ".agents", "skills", name)
		writeTestFile(t, filepath.Join(native, "SKILL.md"), "---\nname: "+name+"\ndescription: Windows restore retry fixture\n---\nPreserve instructions.\n")
		writeTestFile(t, filepath.Join(native, "references", "rules.txt"), "Original rules.\n")
		assets := t.TempDir()
		writeTestFile(t, filepath.Join(assets, "asset.txt"), "Original linked asset.\n")
		junction(t, assets, filepath.Join(native, "assets"))
		skills = append(skills, SkillAdoption{Path: native})
	}
	if err := os.MkdirAll(filepath.Join(home, ".agentcfg"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := CreateInitRestorePoint(home); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeInitLinks(context.Background(), home, nil, skills); err != nil {
		t.Fatal(err)
	}
	point, _, err := readInitPoint(home)
	if err != nil || len(point.Layout) != count {
		t.Fatalf("restore retry fixture: %d entries, want %d: %v", len(point.Layout), count, err)
	}
	return home, point
}

func holdNativeRestoreRename(t *testing.T, path, probe string) *os.File {
	t.Helper()
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	blocked := os.Rename(path, probe)
	if !errors.Is(blocked, windows.ERROR_ACCESS_DENIED) && !errors.Is(blocked, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("fixture did not establish native rename denial: %v", blocked)
	}
	t.Logf("native handle blocks rename: %v", blocked)
	return held
}

func TestNativeLayoutRestoreRejectsDescendantEditsDuringWindowsRetry(t *testing.T) {
	for _, phase := range []string{"native", "original", "missing-native", "rollback"} {
		t.Run(phase, func(t *testing.T) {
			count := 1
			if phase == "rollback" {
				count = 2
			}
			home, point := nativeRestoreRetryFixture(t, count)
			entry := point.Layout[0]
			native := filepath.Join(home, filepath.FromSlash(entry.Path))
			original := filepath.Join(home, ".agentcfg", filepath.FromSlash(entry.Original))
			editRoot, destination := native, filepath.Join(filepath.Dir(original), "restored-copy")
			if phase == "original" || phase == "missing-native" {
				editRoot, destination = original, native
			}
			if phase == "missing-native" {
				if err := os.RemoveAll(native); err != nil {
					t.Fatal(err)
				}
			}
			var foreignStage string
			if phase == "rollback" {
				destination = original
				secondOriginal := filepath.Join(home, ".agentcfg", filepath.FromSlash(point.Layout[1].Original))
				foreignStage = filepath.Join(filepath.Dir(secondOriginal), "restored-copy")
				// Fail the second entry only after the first has been restored.
				writeTestFile(t, foreignStage, "Foreign destination.\n")
			}
			ready, edited, abort := make(chan struct{}), make(chan struct{}), make(chan struct{})
			checks := 0
			signaled := false
			rename := func(ctx context.Context, source, target string, check func() error) error {
				if source != editRoot || target != destination {
					return fsops.Rename(ctx, source, target, check)
				}
				if check == nil {
					return errors.New("restore did not provide a descendant check")
				}
				return fsops.Rename(ctx, source, target, func() error {
					checks++
					if err := check(); err != nil {
						return err
					}
					if !signaled {
						signaled = true
						// The preflight and first descendant check have both succeeded.
						close(ready)
						select {
						case <-edited:
						case <-abort:
							return context.Canceled
						}
					}
					return nil
				})
			}
			finished := make(chan error, 1)
			exited := make(chan struct{})
			var held *os.File
			defer func() {
				close(abort)
				if held != nil {
					_ = held.Close()
				}
				select {
				case <-exited:
				case <-time.After(6 * time.Second):
					t.Error("restore worker did not stop after fixture handles closed")
				}
			}()
			go func() {
				defer close(exited)
				finished <- restoreNativeLayoutWithRename(home, point, rename)
			}()
			select {
			case <-ready:
			case err := <-finished:
				t.Fatalf("restore ended before the first checked %s rename: %v", phase, err)
			case <-time.After(6 * time.Second):
				t.Fatalf("restore did not reach the first checked %s rename", phase)
			}
			held = holdNativeRestoreRename(t, editRoot, filepath.Join(filepath.Dir(original), "lock-probe"))
			before, err := os.Stat(editRoot)
			if err != nil {
				t.Fatal(err)
			}
			asset := filepath.Join(editRoot, "references", "rules.txt")
			manual := []byte("User changed the descendant while the Windows rename was waiting.\n")
			if err := os.WriteFile(asset, manual, 0600); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(editRoot)
			if err != nil {
				t.Fatal(err)
			}
			if !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("fixture unexpectedly changed the root directory mtime")
			}
			t.Logf("descendant changed without changing root mtime %s", after.ModTime())
			// Keep the handle open: the first native attempt must fail, so the
			// next checked attempt sees the completed descendant edit.
			close(edited)
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("restore accepted a descendant edit made during a blocked rename")
				}
				changed := false
				for _, message := range strings.Split(err.Error(), "\n") {
					if message == "native layout changed while waiting to rename: "+editRoot {
						changed = true
					}
				}
				if !changed {
					t.Errorf("restore did not report the edited descendant: %v", err)
				}
				t.Logf("restore rejected the concurrent edit: %v", err)
			case <-time.After(6 * time.Second):
				t.Fatal("restore did not reject the edit while the native handle remained open")
			}
			if checks < 2 {
				t.Errorf("targeted rename ran %d descendant checks, want a check after the blocked attempt", checks)
			}
			if err := held.Close(); err != nil {
				t.Fatal(err)
			}
			held = nil
			assertAdoptionBytes(t, asset, manual)
			if phase != "rollback" {
				if _, err := os.Lstat(original); err != nil {
					t.Fatalf("original recovery entry was consumed despite the conflict: %v", err)
				}
			} else {
				assertAdoptionBytes(t, foreignStage, []byte("Foreign destination.\n"))
			}
		})
	}
}
