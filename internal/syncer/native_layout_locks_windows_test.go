//go:build windows

package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentcfg/internal/fsstate"
)

func TestLegacySkillSwapWithLockedStagingRestoresOriginalLayout(t *testing.T) {
	for _, kind := range []string{"nested-junction", "symlink", "junction"} {
		for _, cancelled := range []bool{false, true} {
			name := kind + "/released"
			if cancelled {
				name = kind + "/cancelled"
			}
			t.Run(name, func(t *testing.T) {
				home := t.TempDir()
				native := filepath.Join(home, ".agents", "skills", "twg-code-review")
				payload := filepath.Join(t.TempDir(), "payload")
				writeTestFile(t, filepath.Join(payload, "SKILL.md"), "---\nname: twg-code-review\ndescription: Windows lock fixture\n---\nPreserve instructions.\n")
				if err := os.MkdirAll(filepath.Dir(native), 0700); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "nested-junction":
					if err := os.Rename(payload, native); err != nil {
						t.Fatal(err)
					}
					resources := t.TempDir()
					writeTestFile(t, filepath.Join(resources, "data.txt"), "Read-only asset.\n")
					if err := os.Chmod(filepath.Join(resources, "data.txt"), 0400); err != nil {
						t.Fatal(err)
					}
					junction(t, resources, filepath.Join(native, "assets"))
				case "symlink":
					pathAliasSymlink(t, payload, native)
				case "junction":
					junction(t, payload, native)
				}
				if err := os.MkdirAll(filepath.Join(home, ".agentcfg"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := CreateInitRestorePoint(home); err != nil {
					t.Fatal(err)
				}
				if err := MaterializeInitLinks(context.Background(), home, nil, []SkillAdoption{{Path: native}}); err != nil {
					t.Fatal(err)
				}
				point, _, err := readInitPoint(home)
				if err != nil || len(point.Layout) != 1 {
					t.Fatalf("layout fixture: %v", err)
				}
				entry := point.Layout[0]
				staged := filepath.Join(home, filepath.FromSlash(entry.Staged))
				original := filepath.Join(home, ".agentcfg", filepath.FromSlash(entry.Original))
				// Replay the actual boundary: completed copy, original still native.
				if err := os.Rename(native, staged); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(original, native); err != nil {
					t.Fatal(err)
				}
				held, err := os.Open(filepath.Join(staged, "SKILL.md"))
				if err != nil {
					t.Fatal(err)
				}
				defer held.Close()
				ctx := context.Background()
				var cancel context.CancelFunc
				done := make(chan struct{})
				if cancelled {
					ctx, cancel = context.WithTimeout(ctx, 80*time.Millisecond)
					defer cancel()
					close(done)
				} else {
					go func() { defer close(done); time.Sleep(140 * time.Millisecond); held.Close() }()
				}
				err = swapPreparedLayout(ctx, home, entry)
				<-done
				held.Close()
				if cancelled {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("lock cancellation: %v", err)
					}
					snapshot, snapErr := fsstate.Capture(native, layoutOptions(false))
					if snapErr != nil || snapshot.State != entry.OriginalHash {
						t.Fatalf("original native entry not restored on cancellation: %v", snapErr)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				restore, err := BuildRestorePlan(home)
				if err != nil {
					t.Fatal(err)
				}
				if err := restore.Apply(); err != nil {
					t.Fatal(err)
				}
				snapshot, err := fsstate.Capture(native, layoutOptions(false))
				if err != nil || snapshot.State != entry.OriginalHash {
					t.Fatalf("original layout changed: %v", err)
				}
			})
		}
	}
}
