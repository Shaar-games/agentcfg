package syncer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAdoptionCanRetryAfterRestoreJournalRollback(t *testing.T) {
	f := newAdoptionFixture(t, "cursor", true)
	if err := CreateInitRestorePoint(f.opts.Home); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(f.opts.Home, ".agentcfg", initRestoreFile)
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	forced := errors.New("fixture failure after restore journal capture")
	plan.extraPreflight = func() error {
		current, err := os.ReadFile(journalPath)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, before) {
			return forced
		}
		return nil
	}
	if err := plan.Apply(); !errors.Is(err, forced) {
		t.Fatalf("did not reach the intended rollback: %v", err)
	}
	assertAdoptionBytes(t, f.target.path, f.before)
	assertAdoptionBytes(t, journalPath, before)
	assertNoAdoptionManifest(t, f.opts.Home)
	if hash, err := SkillTreeSHA256(f.nativeSkill); err != nil || hash != f.opts.AdoptSkills[0].TreeSHA256 {
		t.Fatalf("failed sync changed the original native skill: %s (%v)", hash, err)
	}

	retry, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := retry.Apply(); err != nil {
		t.Fatalf("rolled-back adoption could not be retried: %v", err)
	}
	restore, err := BuildRestorePlan(f.opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err != nil {
		t.Fatal(err)
	}
	assertAdoptionBytes(t, f.target.path, f.before)
	if hash, err := SkillTreeSHA256(f.nativeSkill); err != nil || hash != f.opts.AdoptSkills[0].TreeSHA256 {
		t.Fatalf("retry lost the original native skill backup: %s (%v)", hash, err)
	}
}

func TestAdoptionPreservesMismatchedRestoreSkillBackup(t *testing.T) {
	f := newAdoptionFixture(t, "cursor", true)
	if err := CreateInitRestorePoint(f.opts.Home); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(f.opts.Home, f.nativeSkill)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(f.opts.Home, ".agentcfg", "restore", "skills", rawDigest([]byte(relative))[:16])
	foreign := filepath.Join(backup, "notes.txt")
	writeTestFile(t, foreign, "Preserve this unexpected backup.\n")
	if err := plan.Apply(); err == nil {
		t.Fatal("different existing recovery backup was accepted")
	}
	assertAdoptionBytes(t, foreign, []byte("Preserve this unexpected backup.\n"))
	assertAdoptionBytes(t, f.target.path, f.before)
	assertNoAdoptionManifest(t, f.opts.Home)
}
