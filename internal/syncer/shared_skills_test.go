package syncer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"
)

func sharedSkillFixture(t *testing.T, harnesses []string) (*config.Config, Options, string) {
	t.Helper()
	home, err := pathutil.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(config.Settings{Harnesses: harnesses})
	writeTestFile(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), string(settings))
	writeTestFile(t, filepath.Join(home, ".agentcfg", "mcp.json"), `{"mcpServers":{}}`)
	shared := filepath.Join(home, ".agents", "skills", "review")
	for _, root := range []string{shared, filepath.Join(home, ".agentcfg", "skills", "review")} {
		writeTestFile(t, filepath.Join(root, "SKILL.md"), "---\nname: review\ndescription: Review changes.\n---\nPreserve these instructions.\n")
		writeTestFile(t, filepath.Join(root, "references", "rules.txt"), "Original asset.\n")
	}
	if err := CreateInitRestorePoint(home); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := SkillTreeSHA256(shared)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Root: home, Home: home, Scope: "user", Binary: filepath.Join(home, "agentcfg"), SkillMode: "copy", AdoptExisting: true,
		AdoptSkills: []SkillAdoption{{Harness: "codex", Path: shared, CanonicalName: "review", TreeSHA256: hash}}}
	return cfg, opts, shared
}

func TestSharedImportsMoveToHarnessDirectoriesAndRestore(t *testing.T) {
	for _, harnesses := range [][]string{{"codex", "cursor"}, {"cursor"}} {
		t.Run(strings.Join(harnesses, "+"), func(t *testing.T) {
			cfg, opts, shared := sharedSkillFixture(t, harnesses)
			home := opts.Home
			manual := filepath.Join(home, ".agents", "skills", "unselected", "SKILL.md")
			writeTestFile(t, manual, "Unimported native skill.")
			plan, err := BuildPlan(cfg, opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(shared); !os.IsNotExist(err) {
				t.Fatalf("shared import remains: %v", err)
			}
			assertAdoptionBytes(t, manual, []byte("Unimported native skill."))
			for _, harness := range harnesses {
				path := filepath.Join(home, "."+harness, "skills", "review", "references", "rules.txt")
				assertAdoptionBytes(t, path, []byte("Original asset.\n"))
			}
			if len(harnesses) == 1 {
				if _, err := os.Stat(filepath.Join(home, ".codex", "skills", "review")); !os.IsNotExist(err) {
					t.Fatal("unchecked Codex received a skill")
				}
			}
			opts.AdoptExisting, opts.AdoptSkills = false, nil
			next, err := BuildPlan(cfg, opts)
			if err != nil || len(next.Changes) != 0 {
				t.Fatalf("migration is not idempotent: %v, %+v", err, next)
			}
			restore, err := BuildRestorePlan(home)
			if err != nil {
				t.Fatal(err)
			}
			if err := restore.Apply(); err != nil {
				t.Fatal(err)
			}
			assertAdoptionBytes(t, filepath.Join(shared, "references", "rules.txt"), []byte("Original asset.\n"))
			for _, harness := range harnesses {
				if _, err := os.Stat(filepath.Join(home, "."+harness, "skills", "review")); !os.IsNotExist(err) {
					t.Fatal("restore left a distributed skill behind")
				}
			}
		})
	}
}

func TestSharedMigrationRefusesChangedSourcesAndDestinationCollisions(t *testing.T) {
	for _, mutation := range []string{"native", "canonical", "collision"} {
		t.Run(mutation, func(t *testing.T) {
			cfg, opts, shared := sharedSkillFixture(t, []string{"codex"})
			plan, err := BuildPlan(cfg, opts)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "native":
				writeTestFile(t, filepath.Join(shared, "references", "rules.txt"), "Manual change.")
			case "canonical":
				writeTestFile(t, filepath.Join(cfg.Skills[0].Path, "references", "rules.txt"), "Canonical change.")
			case "collision":
				writeTestFile(t, filepath.Join(opts.Home, ".codex", "skills", "review", "SKILL.md"), "Unmanaged destination.")
			}
			if err := plan.Apply(); err == nil {
				t.Fatal("changed preimage was accepted")
			}
			if _, err := os.Stat(shared); err != nil {
				t.Fatalf("failed migration removed shared source: %v", err)
			}
			assertNoAdoptionManifest(t, opts.Home)
		})
	}
}

func TestPreviouslyManagedSharedSkillsMigrateAndKeepRestoreBaseline(t *testing.T) {
	for _, originalExists := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-install", true: "original-import"}[originalExists], func(t *testing.T) {
			cfg, opts, shared := sharedSkillFixture(t, []string{"codex"})
			if !originalExists {
				if err := os.RemoveAll(shared); err != nil {
					t.Fatal(err)
				}
			}
			opts.AdoptExisting, opts.AdoptSkills = false, nil
			legacy, err := BuildPlan(cfg, opts)
			if err != nil {
				t.Fatal(err)
			}
			// Reproduce the previous release's destination and ownership manifest,
			// including the original snapshot when it had adopted a native skill.
			destination := filepath.Join(opts.Home, ".codex", "skills", "review")
			for i := range legacy.Changes {
				change := &legacy.Changes[i]
				if change.Path != destination {
					continue
				}
				change.Path = shared
				if originalExists {
					change.exists, change.adopted = true, true
					change.skill.beforeHash, err = adoptionTreeFingerprint(shared)
					if err != nil {
						t.Fatal(err)
					}
					change.skill.adoptionHash, err = SkillTreeSHA256(shared)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			legacy.state.Skills[shared] = legacy.state.Skills[destination]
			delete(legacy.state.Skills, destination)
			if err := legacy.Apply(); err != nil {
				t.Fatal(err)
			}
			plan, err := BuildPlan(cfg, opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(shared); !os.IsNotExist(err) {
				t.Fatal("old managed copy remains shared")
			}
			assertAdoptionBytes(t, filepath.Join(destination, "references", "rules.txt"), []byte("Original asset.\n"))
			restore, err := BuildRestorePlan(opts.Home)
			if err != nil {
				t.Fatal(err)
			}
			if err := restore.Apply(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("restore left the new destination behind")
			}
			if originalExists {
				assertAdoptionBytes(t, filepath.Join(shared, "references", "rules.txt"), []byte("Original asset.\n"))
			} else if _, err := os.Lstat(shared); !os.IsNotExist(err) {
				t.Fatal("restore changed the pre-init baseline")
			}
		})
	}
}
