package syncer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func linkedAdoptionFixture(t *testing.T) (adoptionFixture, string, string) {
	t.Helper()
	f := newAdoptionFixture(t, "cursor", true)
	pack := filepath.Join(f.opts.Home, "packs", "review")
	if err := os.MkdirAll(filepath.Dir(pack), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.nativeSkill, pack); err != nil {
		t.Fatal(err)
	}
	target, err := filepath.Rel(filepath.Dir(f.nativeSkill), pack)
	if err != nil {
		t.Fatal(err)
	}
	pathAliasSymlink(t, target, f.nativeSkill)
	f.opts.AdoptSkills[0].Source = pack
	return f, pack, target
}

func TestLinkedAdoptionRejectsUnprovedOrChangedTargets(t *testing.T) {
	for _, mutation := range []string{"unproved", "canonical", "outside", "retarget", "content", "nested-link"} {
		t.Run(mutation, func(t *testing.T) {
			f, pack, _ := linkedAdoptionFixture(t)
			var plan *Plan
			var err error
			if mutation == "retarget" || mutation == "content" {
				plan, err = BuildPlan(f.cfg, f.opts)
				if err != nil {
					t.Fatal(err)
				}
			}
			switch mutation {
			case "unproved":
				f.opts.AdoptSkills[0].Source = ""
			case "canonical", "outside", "retarget":
				target := f.canonicalSkill
				if mutation != "canonical" {
					target = filepath.Join(f.opts.Home, "other", "review")
					if mutation == "outside" {
						target = filepath.Join(t.TempDir(), "review")
					}
					if err := copySkill(pack, target); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Remove(f.nativeSkill); err != nil {
					t.Fatal(err)
				}
				pathAliasSymlink(t, target, f.nativeSkill)
				if mutation != "retarget" {
					f.opts.AdoptSkills[0].Source = target
				}
			case "content":
				writeTestFile(t, filepath.Join(pack, "references", "rules.txt"), "Keep my concurrent edit.")
			case "nested-link":
				pathAliasSymlink(t, f.canonicalSkill, filepath.Join(pack, "nested"))
			}
			if plan == nil {
				_, err = BuildPlan(f.cfg, f.opts)
			} else {
				err = plan.Apply()
			}
			if err == nil {
				t.Fatal("unproved or changed linked import was accepted")
			}
			assertAdoptionBytes(t, f.target.path, f.before)
			assertNoAdoptionManifest(t, f.opts.Home)
			if _, err := os.Readlink(f.nativeSkill); err != nil {
				t.Fatal("rejected import replaced the native link")
			}
		})
	}
}

func addLinkRollbackBarrier(p *Plan, home string) {
	barrier := filepath.Join(home, ".agentcfg", "link-rollback-barrier")
	p.Changes = append(p.Changes,
		Change{Path: barrier, after: []byte("regular file")},
		Change{Path: filepath.Join(barrier, "child.json"), after: []byte("{}")},
	)
}

func TestLinkedAdoptionRollbackAndRecoveryPreserveOriginalLink(t *testing.T) {
	f, pack, target := linkedAdoptionFixture(t)
	if err := CreateInitRestorePoint(f.opts.Home); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	plan.extraPreflight = func() error {
		if info, err := os.Lstat(f.nativeSkill); err == nil && !redirected(info) {
			return errors.New("test failure after installing the physical skill copy")
		}
		return nil
	}
	if err := plan.Apply(); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("expected transaction rollback: %v", err)
	}
	if actual, err := os.Readlink(f.nativeSkill); err != nil || actual != target {
		t.Fatal("failed init lost the original link")
	}
	if _, err := os.Stat(pack); err != nil {
		t.Fatal("failed init removed the link target")
	}
	restore, err := BuildRestorePlan(f.opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err != nil {
		t.Fatal(err)
	}
}

func TestLinkedRestoreRollbackKeepsOriginalLinkBackupForRetry(t *testing.T) {
	f, _, target := linkedAdoptionFixture(t)
	if err := CreateInitRestorePoint(f.opts.Home); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	restore, err := BuildRestorePlan(f.opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	addLinkRollbackBarrier(restore.plan, f.opts.Home)
	if err := restore.Apply(); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("expected restore rollback: %v", err)
	}
	if info, err := os.Lstat(f.nativeSkill); err != nil || redirected(info) {
		t.Fatal("restore rollback did not recover the managed physical copy")
	}
	retry, err := BuildRestorePlan(f.opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	if err := retry.Apply(); err != nil {
		t.Fatal(err)
	}
	if actual, err := os.Readlink(f.nativeSkill); err != nil || actual != target {
		t.Fatal("restore retry did not recover the exact original link")
	}
}

func TestLinkedRestoreRejectsChangedOriginalBackup(t *testing.T) {
	f, _, _ := linkedAdoptionFixture(t)
	if err := CreateInitRestorePoint(f.opts.Home); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	restore, err := BuildRestorePlan(f.opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	backup := ""
	for _, change := range restore.plan.Changes {
		if change.skill != nil && change.skill.restoreLink {
			backup = change.skill.source
		}
	}
	if backup == "" {
		t.Fatal("missing original link backup")
	}
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	pathAliasSymlink(t, f.canonicalSkill, backup)
	if err := restore.Apply(); err == nil {
		t.Fatal("restore accepted a retargeted backup")
	}
	if info, err := os.Lstat(f.nativeSkill); err != nil || redirected(info) {
		t.Fatal("rejected restore replaced the current physical skill copy")
	}
}
