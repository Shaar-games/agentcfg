package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agentcfg/internal/config"
)

func nativeLayoutFixture(t *testing.T, two bool) (adoptionFixture, string, []nativeLayoutEntry) {
	t.Helper()
	f := newAdoptionFixture(t, "cursor", true)
	source := filepath.Join(f.opts.Home, "packs", "shared")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Dir(f.nativeSkill), source); err != nil {
		t.Fatal(err)
	}
	pathAliasSymlink(t, source, filepath.Dir(f.nativeSkill))
	f.opts.AdoptSkills[0].Source = filepath.Join(source, "review")
	harnesses := []string{"cursor"}
	if two {
		codex := filepath.Join(f.opts.Home, ".codex", "skills")
		if err := os.MkdirAll(filepath.Dir(codex), 0700); err != nil {
			t.Fatal(err)
		}
		pathAliasSymlink(t, source, codex)
		writeTestFile(t, filepath.Join(f.opts.Home, ".agentcfg", "agentcfg.json"), `{"harnesses":["cursor","codex"]}`)
		var err error
		f.cfg, err = config.Load(f.opts.Home)
		if err != nil {
			t.Fatal(err)
		}
		f.opts.AdoptSkills = append(f.opts.AdoptSkills, SkillAdoption{Harness: "codex", Path: filepath.Join(codex, "review"), CanonicalName: "review", TreeSHA256: f.opts.AdoptSkills[0].TreeSHA256, Source: filepath.Join(source, "review")})
		harnesses = append(harnesses, "codex")
	}
	if err := CreateInitRestorePoint(f.opts.Home); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeInitLinks(context.Background(), f.opts.Home, harnesses, f.opts.AdoptSkills); err != nil {
		t.Fatal(err)
	}
	point, _, err := readInitPoint(f.opts.Home)
	if err != nil || len(point.Layout) == 0 {
		t.Fatalf("missing native layout snapshot: %v", err)
	}
	return f, source, point.Layout
}

func TestNativeLayoutInterruptedPreparationRecoversOriginalLinks(t *testing.T) {
	for _, kind := range []string{"swap", "partial-copy", "foreign-staging"} {
		t.Run(kind, func(t *testing.T) {
			f, _, entries := nativeLayoutFixture(t, false)
			entry := entries[0]
			root := filepath.Join(f.opts.Home, filepath.FromSlash(entry.Path))
			staged := filepath.Join(f.opts.Home, filepath.FromSlash(entry.Staged))
			original := filepath.Join(f.opts.Home, ".agentcfg", filepath.FromSlash(entry.Original))
			if err := os.Rename(root, staged); err != nil {
				t.Fatal(err)
			}
			if kind != "swap" {
				if err := os.Rename(original, root); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(staged, "review", "references", "rules.txt")); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "foreign-staging" {
				writeTestFile(t, filepath.Join(staged, "user-notes.txt"), "Preserve my notes.")
				if _, err := BuildRestorePlan(f.opts.Home); err == nil {
					t.Fatal("foreign staging data was accepted")
				}
				assertAdoptionBytes(t, filepath.Join(staged, "user-notes.txt"), []byte("Preserve my notes."))
				return
			}
			restore, err := BuildRestorePlan(f.opts.Home)
			if err != nil {
				t.Fatal(err)
			}
			if err := restore.Apply(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Readlink(root); err != nil {
				t.Fatal("original directory link was not restored")
			}
			if _, err := os.Lstat(staged); !os.IsNotExist(err) {
				t.Fatal("owned interrupted staging directory remains")
			}
		})
	}
}

func TestNativeLayoutManagedAndUnmanagedEditsArePreserved(t *testing.T) {
	for _, kind := range []string{"managed", "unmanaged", "backup"} {
		t.Run(kind, func(t *testing.T) {
			f, _, entries := nativeLayoutFixture(t, false)
			plan, err := BuildPlan(f.cfg, f.opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			mcp, err := os.ReadFile(f.target.path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "managed":
				writeTestFile(t, filepath.Join(f.nativeSkill, "SKILL.md"), "Keep manual instructions.")
			case "unmanaged":
				writeTestFile(t, filepath.Join(filepath.Dir(f.nativeSkill), "user-notes.txt"), "Keep my unrelated file.")
			case "backup":
				backup := filepath.Join(f.opts.Home, ".agentcfg", filepath.FromSlash(entries[0].Original))
				if err := os.Remove(backup); err != nil {
					t.Fatal(err)
				}
				pathAliasSymlink(t, f.canonicalSkill, backup)
			}
			if _, err := BuildRestorePlan(f.opts.Home); err == nil {
				t.Fatal("restore accepted an edited native tree or original backup")
			}
			assertAdoptionBytes(t, f.target.path, mcp)
		})
	}
}

func TestNativeLayoutLateSyncFailureRestoresBaseline(t *testing.T) {
	f, _, _ := nativeLayoutFixture(t, false)
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	plan.extraPreflight = func() error {
		current, err := os.ReadFile(f.target.path)
		if err == nil && !bytes.Equal(current, f.before) {
			return errors.New("forced failure after native MCP edit")
		}
		return nil
	}
	if err := plan.Apply(); err == nil {
		t.Fatal("late failure did not interrupt sync")
	}
	restore, err := BuildRestorePlan(f.opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(filepath.Dir(f.nativeSkill)); err != nil {
		t.Fatal("failed initialization did not restore directory link")
	}
	assertAdoptionBytes(t, f.target.path, f.before)
}

func TestNativeLayoutRestoreRollsBackAndCanRetry(t *testing.T) {
	f, _, entries := nativeLayoutFixture(t, true)
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	entry := entries[len(entries)-1]
	barrier := filepath.Join(f.opts.Home, ".agentcfg", filepath.Dir(filepath.FromSlash(entry.Original)), "restored-copy")
	writeTestFile(t, barrier, "Keep this barrier.")
	restore, err := BuildRestorePlan(f.opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err == nil {
		t.Fatal("expected late layout restore failure")
	}
	for _, entry := range entries {
		info, err := os.Lstat(filepath.Join(f.opts.Home, filepath.FromSlash(entry.Path)))
		if err != nil || redirected(info) {
			t.Fatal("failed layout restore did not roll back directory copies")
		}
	}
	assertAdoptionBytes(t, barrier, []byte("Keep this barrier."))
	if err := os.Remove(barrier); err != nil {
		t.Fatal(err)
	}
	retry, err := BuildRestorePlan(f.opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	if err := retry.Apply(); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, err := os.Readlink(filepath.Join(f.opts.Home, filepath.FromSlash(entry.Path))); err != nil {
			t.Fatal("retry did not restore original link")
		}
	}
}

func TestNativeLayoutRejectsOverlappingRestoreTargets(t *testing.T) {
	f, _, _ := nativeLayoutFixture(t, false)
	point, _, err := readInitPoint(f.opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	point.Layout = append(point.Layout, point.Layout[0])
	if err := validateNativeLayout(f.opts.Home, point); err == nil {
		t.Fatal("overlapping native layout restore entries were accepted")
	}
}

func TestNativeLayoutStagingIsPrivateAndLegacyJournalsStillRestore(t *testing.T) {
	f, _, entries := nativeLayoutFixture(t, false)
	entry := entries[0]
	staged := filepath.Join(f.opts.Home, filepath.FromSlash(entry.Staged))
	original := filepath.Join(f.opts.Home, ".agentcfg", filepath.FromSlash(entry.Original))
	if !sameAdoptionPath(filepath.Dir(staged), filepath.Dir(original)) {
		t.Fatal("staging remains in a native harness discovery directory")
	}
	point, _, err := readInitPoint(f.opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	point.Layout[0].Staged = filepath.ToSlash(filepath.Join(filepath.Dir(filepath.FromSlash(entry.Path)), ".agentcfg-layout-legacy"))
	data, err := json.Marshal(point)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(f.opts.Home, ".agentcfg", initRestoreFile), string(data))
	restore, err := BuildRestorePlan(f.opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(filepath.Join(f.opts.Home, filepath.FromSlash(entry.Path))); err != nil {
		t.Fatal("legacy restore journal did not restore original link")
	}
}
