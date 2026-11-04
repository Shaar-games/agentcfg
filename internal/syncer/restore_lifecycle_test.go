package syncer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agentcfg/internal/config"
)

func TestRestoreTracksLaterCanonicalSyncs(t *testing.T) {
	for _, kind := range []string{"mcp", "skill", "new-harness", "plugin-skill", "manual-native-edit", "rollback-new-parent"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdoptionFixture(t, "cursor", kind == "skill")
			home := f.opts.Home
			linkedParent := ""
			if kind == "rollback-new-parent" {
				linkedParent = filepath.Dir(f.target.path)
				target := filepath.Join(t.TempDir(), "cursor")
				if err := os.Rename(linkedParent, target); err != nil {
					t.Fatal(err)
				}
				pathAliasSymlink(t, target, linkedParent)
			}
			if err := CreateInitRestorePoint(home); err != nil {
				t.Fatal(err)
			}
			if err := MaterializeInitLinks(context.Background(), home, []string{"cursor"}, f.opts.AdoptSkills); err != nil {
				t.Fatal(err)
			}
			initial, err := BuildPlan(f.cfg, f.opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := initial.Apply(); err != nil {
				t.Fatal(err)
			}
			journalPath := filepath.Join(home, ".agentcfg", initRestoreFile)
			journalBefore, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "mcp", "manual-native-edit":
				writeTestFile(t, filepath.Join(home, ".agentcfg", "mcp.json"), `{"mcpServers":{"second":{"command":"inert"}}}`)
				if kind == "manual-native-edit" {
					data, err := os.ReadFile(f.target.path)
					if err != nil {
						t.Fatal(err)
					}
					writeTestFile(t, f.target.path, string(bytes.ReplaceAll(data, []byte("untouched"), []byte("keep-my-edit"))))
				}
			case "skill":
				writeTestFile(t, filepath.Join(f.canonicalSkill, "SKILL.md"), "---\nname: review\ndescription: Updated instructions\n---\nNew canonical body.\n")
			case "new-harness":
				writeTestFile(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), `{"harnesses":["cursor","codex"]}`)
			case "plugin-skill":
				root := filepath.Join(home, ".agentcfg", "plugins", "demo")
				writeTestFile(t, filepath.Join(root, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo"}`)
				writeTestFile(t, filepath.Join(root, "skills", "added", "SKILL.md"), "---\nname: added\ndescription: Added after init\n---\nPreserve this canonical skill.\n")
			case "rollback-new-parent":
				writeTestFile(t, filepath.Join(home, ".agentcfg", "skills", "added", "SKILL.md"), "---\nname: added\ndescription: Rollback fixture\n---\nInstructions.\n")
			}
			cfg, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			opts := f.opts
			opts.AdoptExisting = false
			opts.AdoptMCP = nil
			opts.AdoptSkills = nil
			plan, err := BuildPlan(cfg, opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Changes) == 0 {
				t.Fatal("canonical update was not projected")
			}
			if kind == "rollback-new-parent" {
				parent := filepath.Join(home, ".cursor", "skills")
				plan.extraPreflight = func() error {
					if _, err := os.Stat(parent); err == nil {
						return errors.New("forced failure after parent creation")
					}
					return nil
				}
				if err := plan.Apply(); err == nil {
					t.Fatal("forced failure did not happen")
				}
				if _, err := os.Lstat(parent); !os.IsNotExist(err) {
					t.Fatal("rollback left a newly created native parent")
				}
				if after, err := os.ReadFile(journalPath); err != nil || !bytes.Equal(after, journalBefore) {
					t.Fatal("failed sync changed the restore checkpoint")
				}
			} else if err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			restore, err := BuildRestorePlan(home)
			if kind == "manual-native-edit" {
				if err == nil {
					t.Fatal("restore accepted unrelated manual native edits")
				}
				data, _ := os.ReadFile(f.target.path)
				if !bytes.Contains(data, []byte("keep-my-edit")) {
					t.Fatal("manual edit was lost")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := restore.Apply(); err != nil {
				t.Fatal(err)
			}
			assertAdoptionBytes(t, f.target.path, f.before)
			if linkedParent != "" {
				if _, err := os.Readlink(linkedParent); err != nil {
					t.Fatal("original parent link was not restored")
				}
			}
			if kind == "new-harness" {
				if _, err := os.Lstat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
					t.Fatal("new native config was not removed")
				}
			}
			if kind == "plugin-skill" {
				if _, err := os.Lstat(filepath.Join(home, ".cursor", "skills", "added")); !os.IsNotExist(err) {
					t.Fatal("new plugin copy was left behind")
				}
			}
			if kind == "skill" {
				data, err := os.ReadFile(filepath.Join(f.nativeSkill, "SKILL.md"))
				if err != nil || bytes.Contains(data, []byte("New canonical body")) {
					t.Fatal("original skill was not restored")
				}
			}
		})
	}
}

func TestAdoptionRejectsNewSecondaryConfigAfterPreview(t *testing.T) {
	f := newAdoptionFixture(t, "opencode", false)
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	secondary := filepath.Join(f.opts.Home, ".opencode", "opencode.json")
	manual := `{"mcp":{"late":{"type":"local","command":["keep-my-server"]}}}`
	writeTestFile(t, secondary, manual)
	if err := plan.Apply(); err == nil {
		t.Fatal("late native config was ignored")
	}
	assertAdoptionBytes(t, f.target.path, f.before)
	assertAdoptionBytes(t, secondary, []byte(manual))
	assertNoAdoptionManifest(t, f.opts.Home)
}

func TestNormalSyncRejectsAmbiguousNativeJSON(t *testing.T) {
	f := newAdoptionFixture(t, "cursor", false)
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	f.opts.AdoptExisting = false
	f.opts.AdoptMCP = nil
	data, err := os.ReadFile(f.target.path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`{`), []byte(`{"probe":1,"probe":2,`), 1)
	writeTestFile(t, f.target.path, string(data))
	if _, err := BuildPlan(f.cfg, f.opts); err == nil {
		t.Fatal("duplicate native keys were silently merged")
	}
	assertAdoptionBytes(t, f.target.path, data)
}
