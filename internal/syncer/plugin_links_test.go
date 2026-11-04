package syncer

import (
	"os"
	"path/filepath"
	"testing"

	"agentcfg/internal/config"
	"agentcfg/internal/fsstate"
)

func TestLinkedCanonicalAndPluginSourcesProducePhysicalNativeSkills(t *testing.T) {
	testLinkedCanonicalAndPluginSources(t, pathAliasSymlink)
}

func testLinkedCanonicalAndPluginSources(t *testing.T, directoryLink func(*testing.T, string, string)) {
	home := t.TempDir()
	storage := t.TempDir()
	catalog := filepath.Join(t.TempDir(), "catalog")
	pack := t.TempDir()
	resources := t.TempDir()
	skills := t.TempDir()
	link := func(target, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(target); err == nil && info.IsDir() {
			directoryLink(t, target, path)
		} else {
			pathAliasSymlink(t, target, path)
		}
	}
	writeTestFile(t, filepath.Join(storage, "agentcfg.json"), `{"harnesses":["cursor"]}`)
	writeTestFile(t, filepath.Join(resources, "canonical.json"), `{"mcpServers":{"canonical":{"command":"inert"}}}`)
	writeTestFile(t, filepath.Join(resources, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"linked"}`)
	writeTestFile(t, filepath.Join(resources, "mcp.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{"fixture":{"type":"stdio","command":"./bin/runner","args":["literal"]}}}`)
	writeTestFile(t, filepath.Join(resources, "runner"), "Fixture only; never execute.\n")
	writeTestFile(t, filepath.Join(resources, "SKILL.md"), "---\nname: review\ndescription: Linked plugin fixture.\n---\nInstructions.\n")
	writeTestFile(t, filepath.Join(resources, "assets", "rules.txt"), "Linked asset.\n")
	link(filepath.Join(resources, "plugin.json"), filepath.Join(pack, "plugin.json"))
	link(filepath.Join(resources, "mcp.json"), filepath.Join(pack, "mcp.json"))
	link(resources, filepath.Join(pack, "bin"))
	link(filepath.Join(resources, "SKILL.md"), filepath.Join(skills, "review", "SKILL.md"))
	link(filepath.Join(resources, "assets"), filepath.Join(skills, "review", "assets"))
	link(skills, filepath.Join(pack, "skills"))
	link(pack, filepath.Join(catalog, "installed-folder"))
	link(catalog, filepath.Join(storage, "plugins"))
	link(filepath.Join(resources, "canonical.json"), filepath.Join(storage, "mcp.json"))
	link(storage, filepath.Join(home, ".agentcfg"))
	before, err := fsstate.Capture(resources, fsstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home)
	if err != nil || len(cfg.Warnings) != 0 || len(cfg.Servers) != 2 || len(cfg.Skills) != 1 {
		t.Fatalf("linked components did not load: %v %+v", err, cfg)
	}
	opts := Options{Root: home, Home: home, Scope: "user", SkillMode: "copy", Binary: filepath.Join(home, "bin", "agentcfg")}
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".cursor", "skills", "review")
	copy, err := fsstate.Capture(path, fsstate.Options{})
	if err != nil || copy.Layout != emptyLayoutHash {
		t.Fatalf("plugin skill was not expanded: %v", err)
	}
	assertAdoptionBytes(t, filepath.Join(path, "assets", "rules.txt"), []byte("Linked asset.\n"))
	if next, err := BuildPlan(cfg, opts); err != nil || len(next.Changes) != 0 {
		t.Fatalf("linked source sync is not idempotent: %v", err)
	}
	after, err := fsstate.Capture(resources, fsstate.Options{})
	if err != nil || before.State != after.State {
		t.Fatal("sync modified original plugin resources")
	}
	if _, err := os.Readlink(filepath.Join(home, ".agentcfg")); err != nil {
		t.Fatal("canonical directory alias was replaced")
	}
}
