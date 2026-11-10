//go:build windows

package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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
			editRoot := native
			if phase == "original" || phase == "missing-native" {
				editRoot = original
			}
			if phase == "missing-native" {
				if err := os.RemoveAll(native); err != nil {
					t.Fatal(err)
				}
			}
			var held []*os.File
			if phase == "rollback" {
				second := point.Layout[1]
				secondNative := filepath.Join(home, filepath.FromSlash(second.Path))
				secondOriginal := filepath.Join(home, ".agentcfg", filepath.FromSlash(second.Original))
				held = append(held, holdNativeRestoreRename(t, secondNative, filepath.Join(filepath.Dir(secondOriginal), "lock-probe")))
			} else {
				held = append(held, holdNativeRestoreRename(t, editRoot, filepath.Join(filepath.Dir(original), "lock-probe")))
			}
			finished := make(chan error, 1)
			exited := make(chan struct{})
			go func() {
				defer close(exited)
				finished <- restoreNativeLayout(home, point)
			}()
			defer func() {
				for _, handle := range held {
					_ = handle.Close()
				}
				select {
				case <-exited:
				case <-time.After(6 * time.Second):
					t.Error("restore worker did not stop after fixture handles closed")
				}
			}()
			if phase == "rollback" {
				deadline := time.Now().Add(3 * time.Second)
				for {
					if _, err := os.Lstat(original); os.IsNotExist(err) {
						break // The first original has been installed; the second move is held.
					} else if err != nil {
						t.Fatal(err)
					}
					select {
					case err := <-finished:
						t.Fatalf("restore ended before its first entry was installed: %v", err)
					case <-time.After(10 * time.Millisecond):
					}
					if time.Now().After(deadline) {
						t.Fatal("first restore entry was not installed")
					}
				}
				held = append(held, holdNativeRestoreRename(t, native, filepath.Join(filepath.Dir(original), "rollback-probe")))
				secondOriginal := filepath.Join(home, ".agentcfg", filepath.FromSlash(point.Layout[1].Original))
				// A new destination forces the second move to fail without touching it.
				// The first restored entry is then held in its inverse rename.
				writeTestFile(t, filepath.Join(filepath.Dir(secondOriginal), "restored-copy"), "Foreign destination.\n")
			}
			select {
			case err := <-finished:
				t.Fatalf("restore ended before the locked fixture could be edited: %v", err)
			case <-time.After(200 * time.Millisecond):
			}
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
			for _, handle := range held {
				if err := handle.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("restore accepted a descendant edit made during a blocked rename")
				}
				t.Logf("restore rejected the concurrent edit: %v", err)
			case <-time.After(6 * time.Second):
				t.Fatal("restore did not finish after releasing the native handles")
			}
			assertAdoptionBytes(t, asset, manual)
			if phase != "rollback" {
				if _, err := os.Lstat(original); err != nil {
					t.Fatalf("original recovery entry was consumed despite the conflict: %v", err)
				}
			}
		})
	}
}
