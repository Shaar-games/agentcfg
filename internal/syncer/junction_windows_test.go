//go:build windows

package syncer

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func junction(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("junction unavailable: %v: %s", err, output)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !redirected(info) {
		t.Fatal("Windows junction not identified as a reparse point")
	}
}

func TestWindowsTargetJunctionIsRefused(t *testing.T) {
	cfg, opts := fixture(t)
	cfg.Settings.Harnesses = []string{"cursor"}
	outside := t.TempDir()
	junction(t, outside, filepath.Join(cfg.Root, ".cursor"))
	if _, err := BuildPlan(cfg, opts); err == nil {
		t.Fatal("sync traversed a target junction")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("external directory was changed")
	}
}

func TestWindowsNestedSkillJunctionIsRefused(t *testing.T) {
	cfg, opts := fixture(t)
	cfg.Settings.Harnesses = []string{"cursor"}
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "private.txt"), "not skill content")
	junction(t, outside, filepath.Join(cfg.Skills[0].Path, "escape"))
	if _, err := BuildPlan(cfg, opts); err == nil {
		t.Fatal("sync copied through a nested junction")
	}
}
