package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/bootstrap"
	"agentcfg/internal/config"
	"agentcfg/internal/syncer"
)

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestInitImportsSharedSkillsEvenWithCodexUnchecked(t *testing.T) {
	home := t.TempDir()
	shared := filepath.Join(home, ".agents", "skills", "review")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("---\nname: review\ndescription: Review changes.\n---\nOriginal shared instructions.\n")
	if err := os.WriteFile(filepath.Join(shared, "SKILL.md"), original, 0o600); err != nil {
		t.Fatal(err)
	}
	inventory, err := bootstrap.Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := proposalFromNative(inventory)
	if err != nil {
		t.Fatal(err)
	}
	selected := []string{"cursor"}
	if err := writeInitialSources(home, config.Settings{Harnesses: selected}, proposal.Servers, proposal.Skills); err != nil {
		t.Fatal(err)
	}
	mcp, skills, err := initAdoptionProofs(inventory, proposal.Servers, proposal.Skills, selected)
	if err != nil || len(skills) != 1 {
		t.Fatalf("shared import proof was lost: %v, %+v", err, skills)
	}
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Scope: "user", SkillMode: "copy", AdoptExisting: true, AdoptMCP: mcp, AdoptSkills: skills})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(shared); !os.IsNotExist(err) {
		t.Fatal("shared import was not removed")
	}
	if !bytes.Equal(mustRead(t, filepath.Join(home, ".cursor", "skills", "review", "SKILL.md")), original) {
		t.Fatal("Cursor received different skill bytes")
	}
	restore, err := syncer.BuildRestorePlan(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(shared, "SKILL.md")), original) {
		t.Fatal("original shared skill was not restored")
	}
}

func initNativeFixture(t *testing.T) (string, []byte) {
	t.Helper()
	home := t.TempDir()
	before := []byte("{\"theme\":\"keep\",\"mcpServers\":{\"alpha\":{\"command\":\"inert-test-command\"}}}\n")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), before, 0o600); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(home, ".claude", "skills", "review")
	if err := os.MkdirAll(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: review\ndescription: Test fixture\n---\nInert instructions.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inventory, err := bootstrap.Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := proposalFromNative(inventory)
	if err != nil {
		t.Fatal(err)
	}
	// Change only home directory metadata: this must never invalidate init.
	changed := time.Now().Add(-time.Hour)
	if err := os.Chtimes(home, changed, changed); err != nil {
		t.Fatal(err)
	}
	if err := writeInitialSources(home, config.Settings{Harnesses: []string{"claude-code", "cursor"}}, proposal.Servers, proposal.Skills); err != nil {
		t.Fatal(err)
	}
	adoptMCP, adoptSkills, err := initAdoptionProofs(inventory, proposal.Servers, proposal.Skills, []string{"claude-code", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Scope: "user", Binary: filepath.Join(home, "agentcfg"), SkillMode: "auto", AdoptExisting: true, AdoptMCP: adoptMCP, AdoptSkills: adoptSkills})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	return home, before
}

func TestInitRestoreRoundTripPreservesOriginalsAndArchivesUserAdditions(t *testing.T) {
	home, before := initNativeFixture(t)
	addition := filepath.Join(home, ".agentcfg", "notes.txt")
	if err := os.WriteFile(addition, []byte("Keep my later notes."), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := syncer.BuildRestorePlan(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(home, ".claude.json")), before) {
		t.Fatal("original native JSON was not restored exactly")
	}
	for _, path := range []string{filepath.Join(home, ".cursor", "mcp.json"), filepath.Join(home, ".cursor", "skills", "review"), filepath.Join(home, ".agentcfg")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("new init path still exists: %s (%v)", path, err)
		}
	}
	if string(mustRead(t, filepath.Join(plan.Archive, "notes.txt"))) != "Keep my later notes." {
		t.Fatal("canonical additions were lost")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "review", "SKILL.md")); err != nil {
		t.Fatal("original skill is missing:", err)
	}
	if err := writeInitialSources(home, config.Settings{Harnesses: []string{"claude-code"}}, nil, nil); err != nil {
		t.Fatal("cannot initialize again after restore:", err)
	}
}

func TestRestoreRejectsNativeEditsBeforeAndAfterPreview(t *testing.T) {
	for _, afterPreview := range []bool{false, true} {
		home, _ := initNativeFixture(t)
		var plan *syncer.RestorePlan
		var err error
		if afterPreview {
			plan, err = syncer.BuildRestorePlan(home)
			if err != nil {
				t.Fatal(err)
			}
		}
		native := filepath.Join(home, ".claude.json")
		manual := []byte(`{"manual":"keep"}`)
		if err := os.WriteFile(native, manual, 0o600); err != nil {
			t.Fatal(err)
		}
		unchanged := mustRead(t, filepath.Join(home, ".claude", "skills", "review", "SKILL.md"))
		if afterPreview {
			err = plan.Apply()
		} else {
			_, err = syncer.BuildRestorePlan(home)
		}
		if err == nil {
			t.Fatal("manual native edits were accepted")
		}
		if !bytes.Equal(mustRead(t, native), manual) || !bytes.Equal(mustRead(t, filepath.Join(home, ".claude", "skills", "review", "SKILL.md")), unchanged) {
			t.Fatal("restore partially changed native files on conflict")
		}
	}
}

func TestConfigWithoutInitPromptsAndDecliningWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var output, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"config"}, strings.NewReader("n\n"), &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Initialize agentcfg first?") || strings.Contains(output.String(), "MCP servers    ") {
		t.Fatalf("missing initialization gate: %s", output.String())
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("declining initialization wrote files: %v", err)
	}
}

func TestRestorePreviewDoesNotWriteAndYesArchivesCanonicalOnlyInit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := writeInitialSources(home, config.Settings{Harnesses: []string{}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"restore", "--plain"}, {"restore"}, {"restore", "--yes"}} {
		var output, diagnostics bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader(""), &output, &diagnostics); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(filepath.Join(home, ".agentcfg"))
		if args[len(args)-1] == "--yes" {
			if !os.IsNotExist(err) {
				t.Fatal("canonical sources were not archived")
			}
		} else if err != nil {
			t.Fatal("preview archived canonical sources")
		}
	}
}

func TestRestoreArchivesLegacyPartialSetupWithoutTouchingNativeFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.Mkdir(filepath.Join(home, ".agentcfg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".agentcfg", "agentcfg.json"), []byte(`{"harnesses":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	native := []byte(`{"user":"untouched"}`)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), native, 0o600); err != nil {
		t.Fatal(err)
	}
	var output, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"restore", "--yes"}, strings.NewReader(""), &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(home, ".claude.json")), native) {
		t.Fatal("partial setup recovery touched native configuration")
	}
	if err := writeInitialSources(home, config.Settings{Harnesses: []string{}}, nil, nil); err != nil {
		t.Fatal("partial setup still blocks init:", err)
	}
}

func TestRestoreRefusesGuessingOriginalsForLegacySyncedSetup(t *testing.T) {
	home, _ := initNativeFixture(t)
	if err := os.Remove(filepath.Join(home, ".agentcfg", "init-restore.json")); err != nil {
		t.Fatal(err)
	}
	native := mustRead(t, filepath.Join(home, ".claude.json"))
	if _, err := syncer.BuildRestorePlan(home); err == nil || !strings.Contains(err.Error(), "predates restore support") {
		t.Fatalf("missing recovery evidence was not explained: %v", err)
	}
	if !bytes.Equal(native, mustRead(t, filepath.Join(home, ".claude.json"))) {
		t.Fatal("legacy restore guessed and replaced native configuration")
	}
}

func TestConfigPartialSetupNeedsApprovalAndDoesNotArchiveInPipedMode(t *testing.T) {
	for _, answer := range []string{"n\n", "y\n"} {
		home := t.TempDir()
		if err := os.Mkdir(filepath.Join(home, ".agentcfg"), 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(home, ".agentcfg", "agentcfg.json")
		original := []byte(`{"harnesses":["codex"]}`)
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := configure(context.Background(), home, strings.NewReader(answer), &output); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "Incomplete canonical sources") || !bytes.Equal(mustRead(t, path), original) {
			t.Fatal("partial configuration was not explained or was overwritten")
		}
	}
}

func TestInitRefusesChangedNativeContentEvenWithStableDirectory(t *testing.T) {
	home := t.TempDir()
	native := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(native, []byte(`{"mcpServers":{"alpha":{"command":"inert"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	inventory, err := bootstrap.Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := proposalFromNative(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte(`{"mcpServers":{"alpha":{"command":"changed"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runInitWizard(context.Background(), home, proposal, initOptions{Yes: true}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("changed source was silently imported")
	}
	if _, err := os.Stat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
		t.Fatal("stale initialization published canonical sources")
	}
}
