package syncer

import (
	"os"
	"path/filepath"
	"testing"

	"agentmux/internal/config"
)

// An unselected, unmanaged harness is outside this sync's write set. Its
// personal directory may be a link or contain an incompatible native config;
// neither should block safely synchronizing an unrelated selected harness.
func TestUnselectedHarnessSymlinkDoesNotBlockUserSync(t *testing.T) {
	root, home, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("not managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".vscode")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".gemini")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	cfg := &config.Config{
		Root:     root,
		Settings: config.Settings{Harnesses: []string{"codex"}},
		Servers: map[string]config.Server{
			"tools": {Name: "tools", Type: "stdio", Command: "unused", Sharing: "shared"},
		},
	}
	opts := Options{Root: root, Home: home, Scope: "user", Binary: filepath.Join(root, "agentmux")}
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatalf("unselected/unmanaged harness symlink blocked user sync: %v", err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); err != nil {
		t.Fatalf("selected Codex configuration missing: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(outside, "keep.txt"))
	if err != nil || string(contents) != "not managed" {
		t.Fatalf("unselected linked directory changed: %q (%v)", contents, err)
	}
}

// An unselected target is not automatically safe to ignore when AgentMux
// previously owned files there: it still needs to prune those entries, and
// must reject a newly linked directory instead of following it on cleanup.
func TestPreviouslyManagedUnselectedSymlinkIsStillRejected(t *testing.T) {
	root, home, outside := t.TempDir(), t.TempDir(), t.TempDir()
	cfg := &config.Config{
		Root:     root,
		Settings: config.Settings{Harnesses: []string{"cursor"}},
		Servers: map[string]config.Server{
			"tools": {Name: "tools", Type: "stdio", Command: "unused", Sharing: "shared"},
		},
	}
	opts := Options{Root: root, Home: home, Scope: "user", Binary: filepath.Join(root, "agentmux")}
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(home, ".cursor"), filepath.Join(home, ".cursor-preserved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".cursor")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	cfg.Settings.Harnesses = []string{}
	if _, err := BuildPlan(cfg, opts); err == nil {
		t.Fatal("previously owned native config could be pruned through a symlink")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("untrusted linked destination was changed: %v (%v)", entries, err)
	}
}
