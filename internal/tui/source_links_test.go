package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/config"
)

func sourceLink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func TestAvailabilityEditsThroughCanonicalSourceLinks(t *testing.T) {
	for _, kind := range []string{"settings-file", "canonical-directory", "skill-directory", "skill-file", "plugin-directory"} {
		t.Run(kind, func(t *testing.T) {
			body := "---\nname: review\ndescription: Review code\nmetadata:\n  harness: codex, cursor\n---\nBody unchanged.\n"
			root, path, _ := skillSourceFixture(t, body)
			putSettings(t, root, `{"harnesses":["codex","cursor"]}`)
			source := filepath.Join(t.TempDir(), "source")
			logical := path
			switch kind {
			case "settings-file":
				logical = settingsFile(root)
			case "canonical-directory":
				logical = filepath.Join(root, ".agentcfg")
			case "skill-directory":
				logical = filepath.Dir(path)
			case "plugin-directory":
				plugin := filepath.Join(root, ".agentcfg", "plugins", "demo")
				if err := os.MkdirAll(filepath.Join(plugin, "skills"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(plugin, "plugin.json"), []byte(`{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo"}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Dir(path), filepath.Join(plugin, "skills", "review")); err != nil {
					t.Fatal(err)
				}
				logical = plugin
			}
			if err := os.Rename(logical, source); err != nil {
				t.Fatal(err)
			}
			sourceLink(t, source, logical)
			cfg, err := config.Load(root)
			if err != nil || len(cfg.Warnings) != 0 || len(cfg.Skills) != 1 {
				t.Fatalf("linked source did not load: %v %+v", err, cfg)
			}
			if err := setHarnessSelection(root, []string{"cursor"}, cfg.Settings.Harnesses); err != nil {
				t.Fatal(err)
			}
			if err := setSkillHarnessSelection(root, cfg.Skills[0], []string{"cursor"}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Readlink(logical); err != nil {
				t.Fatal("availability editor replaced the source link")
			}
			after, err := config.Load(root)
			if err != nil || len(after.Skills) != 1 || config.Allows(after.Skills[0].Harnesses, "codex") {
				t.Fatalf("linked source availability was not saved: %v %+v", err, after)
			}
			data, err := os.ReadFile(after.Skills[0].File)
			if err != nil || !strings.Contains(string(data), "Body unchanged.") {
				t.Fatal("source edit changed skill body")
			}
		})
	}
}

func TestRetargetedSkillSourceSnapshotIsRejected(t *testing.T) {
	body := "---\nname: review\ndescription: Review\n---\nBody.\n"
	root, path, _ := skillSourceFixture(t, body)
	first := filepath.Join(t.TempDir(), "first.md")
	if err := os.Rename(path, first); err != nil {
		t.Fatal(err)
	}
	sourceLink(t, first, path)
	snapshot := loadSkillFixture(t, root, "review")
	second := filepath.Join(t.TempDir(), "second.md")
	if err := os.WriteFile(second, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	sourceLink(t, second, path)
	if err := setSkillHarnessSelection(root, snapshot, []string{"cursor"}); err == nil {
		t.Fatal("retargeting to identical contents bypassed the selected source binding")
	}
}
