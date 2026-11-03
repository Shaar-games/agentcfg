package syncer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"agentcfg/internal/config"
)

// Installing a skill must not rewrite its content, remove routing metadata,
// or add any file to the copied skill tree.
func TestSkillInstallationPreservesOriginalSKILLmdBytes(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	original := []byte("---\r\nname: review\r\ndescription: Review source code.\r\nmetadata:\r\n  harness: codex,antigravity\r\n  custom: untouched\r\n---\r\n\r\n# Review\r\nPreserve formatting exactly.\r\n")
	source := filepath.Join(root, ".agentcfg", "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Root: root, Skills: []config.Skill{{Name: "review", Path: filepath.Dir(source), Harnesses: []string{"codex", "antigravity"}}},
		Settings: config.Settings{Harnesses: []string{"codex", "antigravity"}},
	}
	plan, err := BuildPlan(cfg, Options{Root: root, Home: home, Scope: "user", Binary: filepath.Join(root, "agentcfg.exe"), SkillMode: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	if err = plan.Apply(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(home, ".agents", "skills", "review", "SKILL.md"),
		filepath.Join(home, ".gemini", "config", "skills", "review", "SKILL.md"),
		source,
	} {
		installed, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(installed, original) {
			t.Errorf("SKILL.md was rewritten at %s", path)
		}
	}
	for _, directory := range []string{
		filepath.Join(home, ".agents", "skills", "review"),
		filepath.Join(home, ".gemini", "config", "skills", "review"),
	} {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "SKILL.md" {
			t.Errorf("copied skill tree differs from source at %s: %v", directory, entries)
		}
	}
}
