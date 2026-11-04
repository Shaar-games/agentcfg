package syncer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/config"
)

func safetyPlan(t *testing.T, home string) *Plan {
	t.Helper()
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(cfg, Options{Root: home, Home: home, Scope: "user", SkillMode: "copy", Binary: filepath.Join(home, "bin", "agentcfg")})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func safetySource(t *testing.T, home string) {
	t.Helper()
	writeTestFile(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), `{"harnesses":["cursor"]}`)
	writeTestFile(t, filepath.Join(home, ".agentcfg", "mcp.json"), `{"mcpServers":{"alpha":{"command":"unused"}}}`)
}

func TestApplyEmptyPlanCreatesNothing(t *testing.T) {
	home := t.TempDir()
	plan := safetyPlan(t, home)
	if len(plan.Changes) != 0 {
		t.Fatalf("expected an empty plan: %+v", plan.Changes)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("empty plan created files: %v (%v)", entries, err)
	}
}

func TestApplyRejectsLinkedLock(t *testing.T) {
	home := t.TempDir()
	safetySource(t, home)
	plan := safetyPlan(t, home)
	external := filepath.Join(t.TempDir(), "external.lock")
	if err := os.Symlink(external, filepath.Join(home, ".agentcfg", "sync.lock")); err != nil {
		t.Skipf("file symlinks unavailable: %v", err)
	}
	if err := plan.Apply(); err == nil {
		t.Error("synchronization followed a symbolic lock file")
	}
	for _, path := range []string{external, filepath.Join(home, ".cursor"), plan.statePath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("rejected lock must not create %s: %v", path, err)
		}
	}
}

func TestApplyRejectsReplacedHome(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	safetySource(t, home)
	plan := safetyPlan(t, home)
	if err := os.Rename(home, filepath.Join(parent, "previous-home")); err != nil {
		t.Fatal(err)
	}
	safetySource(t, home) // Same paths and bytes, different directory identity.
	if err := plan.Apply(); err == nil {
		t.Error("plan for the previous home was applied to its replacement")
	}
	for _, path := range []string{filepath.Join(home, ".cursor"), plan.statePath, filepath.Join(home, ".agentcfg", "sync.lock")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("stale root wrote %s: %v", path, err)
		}
	}
}

// Invoke the writer after a successful precheck, then an external edit. This
// models a writer that does not participate in sync.lock without timing races.
func TestApplyFileRechecksAfterPreflight(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "replace", true: "remove"}[remove], func(t *testing.T) {
			home := t.TempDir()
			safetySource(t, home)
			path := filepath.Join(home, ".cursor", "mcp.json")
			writeTestFile(t, path, `{"mcpServers":{},"theme":"keep"}`)
			plan := safetyPlan(t, home)
			if len(plan.Changes) != 1 {
				t.Fatalf("expected one MCP write: %+v", plan.Changes)
			}
			change := plan.Changes[0]
			change.remove = remove
			if err := plan.checkChange(change); err != nil {
				t.Fatal(err)
			}
			concurrent := []byte(`{"mcpServers":{},"theme":"edited elsewhere"}`)
			if err := os.WriteFile(path, concurrent, 0o600); err != nil {
				t.Fatal(err)
			}
			undo, err := plan.applyChange(change, filepath.Join(home, ".agentcfg", "backups", "previous.json"))
			if err == nil || !strings.Contains(err.Error(), "changed") {
				t.Errorf("concurrent native edit was accepted: %v", err)
			}
			if undo != nil {
				t.Error("a rejected write returned an undo action")
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, concurrent) {
				t.Fatalf("concurrent native edit was lost: %q (%v)", got, err)
			}
		})
	}
}

func TestApplySkillRechecksAfterPreflight(t *testing.T) {
	home := t.TempDir()
	safetySource(t, home)
	source := filepath.Join(home, ".agentcfg", "skills", "review", "SKILL.md")
	writeTestFile(t, source, "---\nname: review\ndescription: Review\n---\nOriginal instructions.\n")
	if err := safetyPlan(t, home).Apply(); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, source, "---\nname: review\ndescription: Review\n---\nNew instructions.\n")
	plan := safetyPlan(t, home)
	if len(plan.Changes) != 1 || plan.Changes[0].skill == nil {
		t.Fatalf("expected one skill update: %+v", plan.Changes)
	}
	change := plan.Changes[0]
	if err := plan.checkChange(change); err != nil {
		t.Fatal(err)
	}
	concurrent := filepath.Join(change.Path, "concurrent.txt")
	writeTestFile(t, concurrent, "Keep this external edit.\n")
	undo, err := plan.applyChange(change, filepath.Join(home, ".agentcfg", "backups", "previous-skill"))
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("concurrent skill edit was accepted: %v", err)
	}
	if undo != nil {
		t.Error("a rejected skill update returned an undo action")
	}
	if got, err := os.ReadFile(concurrent); err != nil || string(got) != "Keep this external edit.\n" {
		t.Fatalf("concurrent skill edit was lost: %q (%v)", got, err)
	}
}

func TestRollbackPreservesExternallyEditedFile(t *testing.T) {
	home := t.TempDir()
	safetySource(t, home)
	path := filepath.Join(home, ".cursor", "mcp.json")
	before := []byte(`{"mcpServers":{},"theme":"keep"}`)
	writeTestFile(t, path, string(before))
	plan := safetyPlan(t, home)
	backup := filepath.Join(home, ".agentcfg", "backups", "previous.json")
	undo, err := plan.applyChange(plan.Changes[0], backup)
	if err != nil || undo == nil {
		t.Fatalf("apply fixture: %v", err)
	}
	concurrent := []byte(`{"external":"must survive rollback"}`)
	if err := os.WriteFile(path, concurrent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := undo(); err == nil {
		t.Fatal("rollback overwrote an external edit")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, concurrent) {
		t.Fatalf("rollback lost external contents: %q (%v)", got, err)
	}
	if got, err := os.ReadFile(backup); err != nil || !bytes.Equal(got, before) {
		t.Fatalf("recovery backup was not preserved: %q (%v)", got, err)
	}
}

func TestRollbackRejectsReplacedHome(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	safetySource(t, home)
	path := filepath.Join(home, ".cursor", "mcp.json")
	writeTestFile(t, path, `{"mcpServers":{},"theme":"keep"}`)
	plan := safetyPlan(t, home)
	change := plan.Changes[0]
	undo, err := plan.applyChange(change, filepath.Join(home, ".agentcfg", "backups", "previous.json"))
	if err != nil || undo == nil {
		t.Fatalf("apply fixture: %v", err)
	}
	if err := os.Rename(home, filepath.Join(parent, "previous-home")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, string(change.after))
	if err := undo(); err == nil {
		t.Fatal("rollback restored the original preimage into a replacement home")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, change.after) {
		t.Fatalf("rollback changed the replacement home: %q (%v)", got, err)
	}
}
