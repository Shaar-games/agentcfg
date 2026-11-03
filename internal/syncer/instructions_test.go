package syncer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/config"
)

func testInstructionsFixture(t *testing.T, text string, harnesses []string) (*config.Config, Options, string) {
	t.Helper()
	home := t.TempDir()
	root := home
	source := filepath.Join(root, ".agentcfg", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return &config.Config{Root: root, Servers: map[string]config.Server{}, Settings: config.Settings{Harnesses: harnesses}}, Options{
		Root: root, Home: home, Scope: "user", Binary: filepath.Join(root, "agentcfg.exe"), SkillMode: "copy",
	}, source
}

func TestGlobalInstructionsRouteSelectedClientsAndStripMetadata(t *testing.T) {
	body := []byte("# Working rules\r\n\r\nUse explicit checks.\r\n")
	canonical := "---\r\nmetadata:\r\n  harness: codex,opencode\r\n---\r\n" + string(body)
	cfg, opts, source := testInstructionsFixture(t, canonical, []string{"claude-code", "codex", "opencode"})
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 2 {
		t.Fatalf("expected two global instructions, got %d: %+v", len(plan.Changes), plan.Changes)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{filepath.Join(opts.Home, ".codex", "AGENTS.md"), filepath.Join(opts.Home, ".config", "opencode", "AGENTS.md")} {
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("%s: generated content changed %q", target, got)
		}
	}
	if _, err := os.Stat(filepath.Join(opts.Home, ".claude", "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatal("Claude Code should be excluded by harness filter")
	}
	if got, _ := os.ReadFile(source); string(got) != canonical {
		t.Fatal("source AGENTS.md modified")
	}
	again, err := BuildPlan(cfg, opts)
	if err != nil || len(again.Changes) != 0 {
		t.Fatalf("instructions not idempotent: %v / %+v", err, again)
	}
}

func TestGlobalInstructionPlainMarkdownAndNone(t *testing.T) {
	cfg, opts, source := testInstructionsFixture(t, "# Common instructions\nDo not rewrite these.\n", []string{"claude-code", "codex", "opencode"})
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 3 {
		t.Fatalf("plain Markdown must target three harnesses: %+v", plan.Changes)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("---\nmetadata:\n  harness: none\n---\n# Ignored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	remove, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(remove.Changes) != 3 {
		t.Fatalf("expected three managed removals: %+v", remove.Changes)
	}
	if err := remove.Apply(); err != nil {
		t.Fatal(err)
	}
	again, err := BuildPlan(cfg, opts)
	if err != nil || len(again.Changes) != 0 {
		t.Fatalf("none is not idempotent: %v %+v", err, again)
	}
}

func TestGlobalInstructionConflictsAndConcurrentSourceEdit(t *testing.T) {
	cfg, opts, source := testInstructionsFixture(t, "# Shared rules\n", []string{"codex"})
	native := filepath.Join(opts.Home, ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(native), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("user owned\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPlan(cfg, opts); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("unowned file was adopted: %v", err)
	}
	if err := os.Remove(native); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("changed after preview\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err == nil || !strings.Contains(err.Error(), "source instructions changed") {
		t.Fatalf("stale source not detected: %v", err)
	}
	if _, err := os.Stat(native); !os.IsNotExist(err) {
		t.Fatal("stale plan wrote instructions")
	}
}

func TestGlobalInstructionsFailOnMalformedFrontmatter(t *testing.T) {
	for _, value := range []string{
		"---\nmetadata:\n  harness: [codex]\n---\n",
		"---\nmetadata:\n  harness: not-a-harness\n---\n",
		"---\nmetadata:\n  harness: codex,,claude-code\n---\n",
		"---\nmetadata:\n  harness: codex\n  harness: opencode\n---\n",
		"---\nmetadata: broken\n---\n",
	} {
		t.Run(strings.ReplaceAll(value, "\n", "_"), func(t *testing.T) {
			cfg, opts, _ := testInstructionsFixture(t, value, []string{"codex"})
			if _, err := BuildPlan(cfg, opts); err == nil {
				t.Fatal("invalid AGENTS.md frontmatter accepted")
			}
		})
	}
}

func TestGlobalInstructionPreservesOtherYAMLAndComments(t *testing.T) {
	source := "---\r\n# published metadata\r\ntitle: Project guide\r\nmetadata:\r\n  harness: codex,claude-code # agentcfg routing\r\n  owner: engineering\r\nsummary: Keep legacy conventions\r\n---\r\n\r\n# Instructions\r\nUse tests.\r\n"
	expected := "---\r\n# published metadata\r\ntitle: Project guide\r\nmetadata:\r\n  owner: engineering\r\nsummary: Keep legacy conventions\r\n---\r\n\r\n# Instructions\r\nUse tests.\r\n"
	out, filter, err := instructionsContent([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != expected {
		t.Errorf("unrelated YAML was removed or reformatted:\nEXPECTED:\n%q\nGOT:\n%q", expected, out)
	}
	if len(filter) != 2 || filter[0] != "codex" || filter[1] != "claude-code" {
		t.Fatalf("incorrect metadata.harness parse: %v", filter)
	}
	withoutRouting := []byte("---\ntitle: No routing metadata\n---\n# Body\n")
	unchanged, filter, err := instructionsContent(withoutRouting)
	if err != nil || len(filter) != 0 || !bytes.Equal(unchanged, withoutRouting) {
		t.Fatalf("unrelated YAML should be preserved: %v %q", err, unchanged)
	}
}

func TestGlobalInstructionRejectsAmbiguousYAMLLayouts(t *testing.T) {
	for _, value := range []string{
		"---\nmetadata: {harness: codex}\n---\n# Body\n",
		"---\nmetadata:\n  harness: |\n    codex\n---\n# Body\n",
	} {
		_, _, err := instructionsContent([]byte(value))
		if err == nil {
			t.Fatalf("ambiguous metadata.harness layout was accepted: %q", value)
		}
	}
}
