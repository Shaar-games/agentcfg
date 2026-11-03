package tui

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentcfg/internal/config"
)

func skillSourceFixture(t *testing.T, body string) (string, string, config.Skill) {
	t.Helper()
	root := t.TempDir()
	settings := filepath.Join(root, ".agentcfg", "agentcfg.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"harnesses":["codex","antigravity","cursor"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".agentcfg", "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) > 0 || len(cfg.Skills) != 1 {
		t.Fatalf("invalid skill fixture, skills=%+v warnings=%v", cfg.Skills, cfg.Warnings)
	}
	return root, path, cfg.Skills[0]
}

func loadSkillFixture(t *testing.T, root, name string) config.Skill {
	t.Helper()
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, skill := range cfg.Skills {
		if skill.Name == name {
			return skill
		}
	}
	t.Fatalf("skill %q missing; warnings=%v", name, cfg.Warnings)
	return config.Skill{}
}

func TestSkillCommaFilterMinimalLineEditAndBackup(t *testing.T) {
	before := "---\nname: review # do not alter\ndescription: Checks code\nmetadata: # keep note\n  author: alice # keep this\n  harness: 'codex, antigravity' # preserve comment\n  version: '1.0'\n---\n# Review\n\nBody: keep **identical**\n"
	root, path, skill := skillSourceFixture(t, before)
	originalInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := setSkillHarness(root, skill, "antigravity", false); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(before, "harness: 'codex, antigravity'", "harness: 'codex'", 1)
	if string(content) != want {
		t.Fatalf("minimal line edit altered unrelated content\ngot:  %q\nwant: %q", string(content), want)
	}
	if !reflect.DeepEqual(loadSkillFixture(t, root, "review").Harnesses, []string{"codex"}) {
		t.Fatal("changed filter did not load correctly")
	}
	backups, err := filepath.Glob(filepath.Join(root, ".agentcfg", "backups", "skills", "review-*.bak"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("expected one SKILL.md backup: %v %v", backups, err)
	}
	backedUp, err := os.ReadFile(backups[0])
	if err != nil || string(backedUp) != before {
		t.Fatalf("backup differs from pre-edit file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != originalInfo.Mode().Perm() {
		t.Fatalf("file permissions changed unexpectedly: original=%v current=%v (%v)", originalInfo.Mode().Perm(), info.Mode().Perm(), err)
	}
	// Duplicate modal action against an old snapshot must not write again.
	if err := setSkillHarness(root, skill, "antigravity", false); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("expected stale skill snapshot error, got %v", err)
	}
}

func TestSkillNoneSentinelRoundTrip(t *testing.T) {
	initial := "---\nname: review\ndescription: Review source\nmetadata:\n  harness: codex\n---\n# Body\n"
	root, path, skill := skillSourceFixture(t, initial)
	if err := setSkillHarness(root, skill, "codex", false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "metadata:\n  harness: none\n") {
		t.Fatalf("disabling last harness should keep literal metadata.harness: none, got %q (%v)", data, err)
	}
	skill = loadSkillFixture(t, root, "review")
	if !reflect.DeepEqual(skill.Harnesses, []string{"none"}) || config.Allows(skill.Harnesses, "codex") {
		t.Fatalf("none sentinel does not represent disabled access: %+v", skill)
	}
	if err := setSkillHarness(root, skill, "antigravity", true); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "  harness: antigravity\n") {
		t.Fatalf("enabling from none should write selected ID: %q (%v)", data, err)
	}
	skill = loadSkillFixture(t, root, "review")
	if !reflect.DeepEqual(skill.Harnesses, []string{"antigravity"}) {
		t.Fatalf("unexpected filter after enabling: %v", skill.Harnesses)
	}
}

func TestSkillMultiSelectAtomicEditAndEmptySelection(t *testing.T) {
	before := "---\nname: review\ndescription: Checks code\nmetadata: # retain\n  harness: 'codex, antigravity' # keep comment\n  author: alice\n---\n# Body unchanged\n"
	root, path, snapshot := skillSourceFixture(t, before)
	if err := setSkillHarnessSelection(root, snapshot, []string{"codex", "cursor", "gemini"}); err != nil {
		t.Fatal(err)
	}
	got := string(mustReadSkill(t, path))
	expected := strings.Replace(before, "'codex, antigravity'", "'codex, cursor, gemini'", 1)
	if got != expected {
		t.Fatalf("multi-select changed unrelated content:\ngot %q\nwant %q", got, expected)
	}
	backups, err := filepath.Glob(filepath.Join(root, ".agentcfg", "backups", "skills", "review-*.bak"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("expected one backup for entire multi-select: %v (%v)", backups, err)
	}
	if err := setSkillHarnessSelection(root, snapshot, []string{"cursor"}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("stale multi-select unexpectedly succeeded: %v", err)
	}
	snapshot = loadSkillFixture(t, root, "review")
	if err := setSkillHarnessSelection(root, snapshot, nil); err != nil {
		t.Fatal(err)
	}
	if got = string(mustReadSkill(t, path)); !strings.Contains(got, "harness: 'none' # keep comment") || !strings.HasSuffix(got, "# Body unchanged\n") {
		t.Fatalf("clearing all harnesses failed to keep literal none and body: %q", got)
	}
	snapshot = loadSkillFixture(t, root, "review")
	if err := setSkillHarnessSelection(root, snapshot, []string{"antigravity"}); err != nil {
		t.Fatal(err)
	}
	if got = string(mustReadSkill(t, path)); !strings.Contains(got, "harness: 'antigravity' # keep comment") {
		t.Fatalf("multi-select from none did not enable chosen harness: %q", got)
	}
}

func TestSkillMultiSelectPreservesNoChangeAndRejectsBadChoices(t *testing.T) {
	root, path, snapshot := skillSourceFixture(t, "---\nname: review\ndescription: Review source\n---\n# Keep\n")
	if err := setSkillHarnessSelection(root, snapshot, knownSkillHarnesses); !errors.Is(err, errNoChange) {
		t.Fatalf("all-selected default must not rewrite SKILL.md: %v", err)
	}
	for _, selected := range [][]string{{"unknown"}, {"codex", "codex"}} {
		if err := setSkillHarnessSelection(root, snapshot, selected); err == nil {
			t.Fatalf("invalid multi-select accepted: %v", selected)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".agentcfg", "backups")); !os.IsNotExist(err) {
		t.Fatalf("no-op or invalid edit wrote backup directory: %v", err)
	}
	if got := string(mustReadSkill(t, path)); got != "---\nname: review\ndescription: Review source\n---\n# Keep\n" {
		t.Fatal("invalid edit changed source")
	}
}

func mustReadSkill(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSkillToggleFromNoFilterPreservesEverythingElse(t *testing.T) {
	initial := "---\nname: review\ndescription: Review source\nmetadata:\n  author: someone\n---\n# Keep body\n"
	root, path, skill := skillSourceFixture(t, initial)
	if len(skill.Harnesses) != 0 {
		t.Fatal("expected a skill available to every harness")
	}
	if err := setSkillHarness(root, skill, "codex", false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(initial, "metadata:\n", "metadata:\n  harness: claude-code, cursor, opencode, gemini, antigravity, vscode\n", 1)
	if string(data) != want {
		t.Fatalf("unexpected edit of unfiltered skill\ngot:  %q\nwant: %q", data, want)
	}
	loaded := loadSkillFixture(t, root, "review")
	if config.Allows(loaded.Harnesses, "codex") || !config.Allows(loaded.Harnesses, "antigravity") || !config.Allows(loaded.Harnesses, "vscode") {
		t.Fatalf("inherited default harness availability was not preserved: %v", loaded.Harnesses)
	}
	// Missing metadata must be added as a minimal two-line insertion.
	noMetadata := "---\nname: review\ndescription: Review source\n---\n# Keep body\n"
	root2, path2, skill2 := skillSourceFixture(t, noMetadata)
	if err := setSkillHarness(root2, skill2, "codex", false); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path2)
	if err != nil || !strings.Contains(string(data), "metadata:\n  harness: claude-code, cursor, opencode, gemini, antigravity, vscode\n---") {
		t.Fatalf("missing metadata was not created correctly: %q (%v)", data, err)
	}
}

func TestSkillEditMaintainsCRLFAndBOM(t *testing.T) {
	initial := "\ufeff---\r\nname: review\r\ndescription: Review source\r\nmetadata:\r\n  harness: codex, cursor   # note\r\n  purpose: test\r\n---\r\n# Body\r\n"
	root, path, skill := skillSourceFixture(t, initial)
	if err := setSkillHarness(root, skill, "cursor", false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(initial, "harness: codex, cursor", "harness: codex", 1)
	if string(data) != want {
		t.Fatalf("BOM or CRLF not preserved\ngot:  %q\nwant: %q", data, want)
	}
}

func TestSkillMalformedOrComplexYAMLRejectedWithoutModification(t *testing.T) {
	cases := []struct{ name, content string }{
		{"flow-map", "---\nname: review\ndescription: Review source\nmetadata: {harness: codex, owner: alice}\n---\nbody\n"},
		{"quoted-key", "---\nname: review\ndescription: Review source\nmetadata:\n  'harness': codex\n---\nbody\n"},
		{"block-scalar", "---\nname: review\ndescription: Review source\nmetadata:\n  harness: >-\n    codex\n---\nbody\n"},
		{"duplicate-metadata", "---\nname: review\ndescription: Review source\nmetadata:\n  harness: codex\nmetadata:\n  owner: alice\n---\nbody\n"},
		{"list-value", "---\nname: review\ndescription: Review source\nmetadata:\n  harness: [codex, antigravity]\n---\nbody\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(tc.content)
			changed, err := editSkillFrontmatter(data, []string{"codex"}, "codex", false)
			if err == nil {
				t.Fatalf("ambiguous YAML was edited: %q", changed)
			}
			if !bytes.Equal(data, []byte(tc.content)) {
				t.Fatal("input unexpectedly mutated")
			}
		})
	}
}

func TestSkillPluginSourceEditingAndProjectRejection(t *testing.T) {
	root := t.TempDir()
	settingsPath := filepath.Join(root, ".agentcfg", "agentcfg.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"harnesses":["codex","cursor"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(root, ".agentcfg", "plugins", "demo")
	manifestPath := filepath.Join(plugin, "plugin.json")
	path := filepath.Join(plugin, "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo","description":"Demo"}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	skillBody := "---\nname: review\ndescription: Review plugin files\nmetadata:\n  harness: codex, cursor\n---\n# The plugin body\n"
	if err := os.WriteFile(path, []byte(skillBody), 0o600); err != nil {
		t.Fatal(err)
	}
	skill := loadSkillFixture(t, root, "demo.review")
	if err := setSkillHarness(root, skill, "cursor", false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != strings.Replace(skillBody, "harness: codex, cursor", "harness: codex", 1) {
		t.Fatalf("plugin skill edit changed unrelated content: %q (%v)", got, err)
	}
	manifestAfter, err := os.ReadFile(manifestPath)
	if err != nil || string(manifestAfter) != manifest {
		t.Fatal("plugin manifest was modified")
	}
	if _, err := checkedSkillSource(root, config.Skill{Name: "fake", Path: filepath.Join(root, "project", "skills", "review")}); err == nil || !strings.Contains(err.Error(), "project edits are forbidden") {
		t.Fatalf("project skill source should be rejected: %v", err)
	}
}

func TestSkillUnsupportedHarnessAndSourceStaleRejection(t *testing.T) {
	initial := "---\nname: review\ndescription: Review source\nmetadata:\n  harness: codex\n---\n"
	root, path, skill := skillSourceFixture(t, initial)
	if err := setSkillHarness(root, skill, "vscode", true); err == nil {
		t.Fatal("VS Code user scope should be rejected")
	}
	if err := setSkillHarness(root, skill, "unknown", true); err == nil {
		t.Fatal("unknown harness should be rejected")
	}
	if err := setSkillHarness(root, skill, "codex", false); err != nil {
		t.Fatal(err)
	}
	if err := setSkillHarness(root, skill, "codex", false); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("stale filter snapshot accepted: %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(current), "harness: none") {
		t.Fatal("last harness change did not persist")
	}
	// Editing a skill in another root is never authorized by a snapshot path.
	skill.Path = filepath.Join(t.TempDir(), "SKILL.md")
	if err := setSkillHarness(root, skill, "cursor", true); err == nil {
		t.Fatal("stale/different source path was accepted")
	}
}
