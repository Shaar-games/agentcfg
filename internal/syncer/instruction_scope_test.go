package syncer

import (
	"os"
	"path/filepath"
	"testing"

	"agentcfg/internal/config"
)

func TestUnusedInstructionTargetsDoNotBlockUserSync(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
	}{
		{name: "no canonical instructions"},
		{name: "filtered canonical instructions", source: "---\nmetadata:\n  harness: none\n---\nKeep this text.\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, outside := t.TempDir(), t.TempDir()
			writeTestFile(t, filepath.Join(outside, "keep.txt"), "Not managed.\n")
			for _, relative := range []string{".claude", ".codex", ".config/opencode"} {
				destination := filepath.Join(home, filepath.FromSlash(relative))
				if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, destination); err != nil {
					t.Skipf("directory symlinks unavailable: %v", err)
				}
			}
			if test.source != "" {
				writeTestFile(t, filepath.Join(home, ".agentcfg", "AGENTS.md"), test.source)
			}
			cfg := &config.Config{Root: home, Settings: config.Settings{Harnesses: []string{"cursor"}}, Servers: map[string]config.Server{
				"tools": {Name: "tools", Type: "stdio", Command: "unused", Sharing: "shared"},
			}}
			plan, err := BuildPlan(cfg, Options{Root: home, Home: home, Scope: "user", Binary: filepath.Join(home, "agentcfg")})
			if err != nil {
				t.Fatalf("unused instruction target blocked Cursor sync: %v", err)
			}
			if err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(home, ".cursor", "mcp.json")); err != nil {
				t.Fatalf("selected Cursor configuration missing: %v", err)
			}
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 1 || entries[0].Name() != "keep.txt" {
				t.Fatalf("unused linked destinations changed: %v (%v)", entries, err)
			}
			if contents, err := os.ReadFile(filepath.Join(outside, "keep.txt")); err != nil || string(contents) != "Not managed.\n" {
				t.Fatalf("unused destination contents changed: %q (%v)", contents, err)
			}
		})
	}
}

func TestUnselectedOwnedInstructionTargetStillRejectsSymlink(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(home, ".agentcfg", "AGENTS.md"), "Original instructions.\n")
	cfg := &config.Config{Root: home, Settings: config.Settings{Harnesses: []string{"claude-code"}}}
	opts := Options{Root: home, Home: home, Scope: "user", Binary: filepath.Join(home, "agentcfg")}
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(home, ".claude"), filepath.Join(home, ".claude-before")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".claude")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	cfg.Settings.Harnesses = nil
	if _, err := BuildPlan(cfg, opts); err == nil {
		t.Fatal("cleanup of owned instructions followed a replaced directory")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("rejected cleanup changed external destination: %v (%v)", entries, err)
	}
}
