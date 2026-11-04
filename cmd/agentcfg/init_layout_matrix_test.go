package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"agentcfg/internal/bootstrap"
	"agentcfg/internal/config"
	"agentcfg/internal/fsstate"
	"agentcfg/internal/pathutil"
	"agentcfg/internal/syncer"
)

var layoutSkillRoots = []string{".claude/skills", ".cursor/skills", ".agents/skills", ".codex/skills", ".config/opencode/skills", ".opencode/skills", ".gemini/skills", ".gemini/config/skills"}
var layoutSkillOwners = map[string]string{
	".claude/skills": "claude-code", ".cursor/skills": "cursor",
	".agents/skills": "codex", ".codex/skills": "codex",
	".config/opencode/skills": "opencode", ".opencode/skills": "opencode",
	".gemini/skills": "gemini", ".gemini/config/skills": "antigravity",
}

const layoutSkill = "---\nname: layout\ndescription: Shared directory fixture.\n---\nKeep these instructions.\n"

func TestInitSkillDirectorySymlinkMatrix(t *testing.T) {
	for _, relative := range []bool{false, true} {
		name := "absolute"
		if relative {
			name = "relative"
		}
		t.Run(name, func(t *testing.T) {
			testSkillDirectoryMatrix(t, func(t *testing.T, target, path string) {
				if relative {
					var err error
					target, err = filepath.Rel(filepath.Dir(path), target)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("directory symlink unavailable: %v", err)
				}
			})
		})
	}
}

func testSkillDirectoryMatrix(t *testing.T, link nativeSkillLink) {
	for _, alias := range layoutSkillRoots {
		for _, source := range layoutSkillRoots {
			if alias == source {
				continue
			}
			t.Run(alias+"-to-"+source, func(t *testing.T) {
				t.Parallel()
				home := t.TempDir()
				target := filepath.Join(home, filepath.FromSlash(source))
				path := filepath.Join(home, filepath.FromSlash(alias))
				writeScopeFixture(t, filepath.Join(target, "layout", "SKILL.md"), layoutSkill)
				writeScopeFixture(t, filepath.Join(target, "layout", "assets", "data.txt"), "Fixture asset.\n")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				link(t, target, path)
				original, err := os.Readlink(path)
				if err != nil {
					t.Fatal(err)
				}
				harnesses := []string{layoutSkillOwners[alias]}
				if owner := layoutSkillOwners[source]; owner != harnesses[0] {
					harnesses = append(harnesses, owner)
				}
				runLayoutRoundTrip(t, home, harnesses, func() {
					info, err := os.Lstat(path)
					if err != nil || !info.IsDir() || pathutil.IsRedirected(info) {
						t.Fatalf("directory alias was not detached: %s: %v", alias, err)
					}
				})
				if restored, err := os.Readlink(path); err != nil || restored != original {
					t.Fatalf("directory alias was not restored: %v", err)
				}
				if string(mustRead(t, filepath.Join(path, "layout", "SKILL.md"))) != layoutSkill {
					t.Fatal("restored shared skill is incomplete")
				}
			})
		}
	}
}

func runLayoutRoundTrip(t *testing.T, home string, harnesses []string, after func()) {
	t.Helper()
	inventory, err := bootstrap.Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := proposalFromNative(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Skills) == 0 {
		t.Fatalf("no native skills discovered: %+v", inventory.Warnings)
	}
	binary := ""
	if len(proposal.Servers) > 0 {
		binary = cliBinary(t)
	}
	if err := runInitWizard(context.Background(), home, proposal, initOptions{Yes: true, Harnesses: harnesses, Binary: binary}, nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home)
	if err != nil || len(cfg.Warnings) != 0 {
		t.Fatalf("invalid canonical source: %v %+v", err, cfg)
	}
	if after != nil {
		after()
	}
	plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Scope: "user", SkillMode: "copy", Binary: binary})
	if err != nil || len(plan.Changes) != 0 {
		t.Fatalf("subsequent sync is not idempotent: %v", err)
	}
	restore, err := syncer.BuildRestorePlan(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err != nil {
		t.Fatal(err)
	}
}

func TestInitMixedNativeLayoutLinks(t *testing.T) {
	testInitMixedNativeLayout(t, func(t *testing.T, target, path string) {
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	})
}

func testInitMixedNativeLayout(t *testing.T, link nativeSkillLink) {
	for _, kind := range []string{"directory-chain", "reverse-chain", "nested-assets", "skill-file", "mcp-file", "mcp-parent", "mcp-shared", "codex-file", "opencode-parent", "external-skills", "unselected-target", "home-alias"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			claude := filepath.Join(home, ".claude", "skills")
			cursor := filepath.Join(home, ".cursor", "skills")
			shared := filepath.Join(home, ".agents", "skills")
			writeScopeFixture(t, filepath.Join(shared, "layout", "SKILL.md"), layoutSkill)
			makeLink := func(target, path string) {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				link(t, target, path)
			}
			var linked []string
			harnesses := []string{"claude-code", "cursor", "codex"}
			switch kind {
			case "directory-chain":
				makeLink(shared, claude)
				makeLink(claude, cursor)
				linked = []string{claude, cursor}
			case "reverse-chain":
				makeLink(shared, cursor)
				makeLink(cursor, claude)
				linked = []string{cursor, claude}
			case "nested-assets":
				target := filepath.Join(t.TempDir(), "assets")
				writeScopeFixture(t, filepath.Join(target, "file.txt"), "External asset.\n")
				path := filepath.Join(shared, "layout", "assets")
				makeLink(target, path)
				linked = []string{path}
			case "skill-file":
				target := filepath.Join(t.TempDir(), "SKILL.md")
				writeScopeFixture(t, target, layoutSkill)
				path := filepath.Join(shared, "layout", "SKILL.md")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				makeLink(target, path)
				linked = []string{path}
			case "mcp-file", "mcp-shared":
				target := filepath.Join(t.TempDir(), "mcp.json")
				writeScopeFixture(t, target, `{"theme":"keep","mcpServers":{"layout":{"command":"inert","env":{"MODE":"demo"}}}}`)
				path := filepath.Join(home, ".cursor", "mcp.json")
				makeLink(target, path)
				linked = []string{path}
				if kind == "mcp-shared" {
					claudeFile := filepath.Join(home, ".claude.json")
					makeLink(path, claudeFile)
					linked = append(linked, claudeFile)
				}
			case "mcp-parent":
				target := t.TempDir()
				writeScopeFixture(t, filepath.Join(target, "mcp.json"), `{"theme":"keep","mcpServers":{"layout":{"command":"inert"}}}`)
				writeScopeFixture(t, filepath.Join(target, "preferences.txt"), "Keep unrelated preferences.\n")
				path := filepath.Join(home, ".cursor")
				makeLink(target, path)
				linked = []string{path}
			case "codex-file":
				target := filepath.Join(t.TempDir(), "config.toml")
				writeScopeFixture(t, target, "model = \"keep-model\"\n[mcp_servers.layout]\ncommand = \"inert\"\nargs = []\n")
				path := filepath.Join(home, ".codex", "config.toml")
				makeLink(target, path)
				linked = []string{path}
			case "opencode-parent":
				target := t.TempDir()
				writeScopeFixture(t, filepath.Join(target, "opencode.jsonc"), "// Keep this preference.\n"+`{"model":"keep/model","mcp":{"layout":{"type":"local","command":["inert"]}}}`)
				writeScopeFixture(t, filepath.Join(target, "preferences.txt"), "Keep preferences.\n")
				path := filepath.Join(home, ".config", "opencode")
				makeLink(target, path)
				linked = []string{path}
				harnesses = append(harnesses, "opencode")
			case "home-alias":
				path := filepath.Join(t.TempDir(), "home")
				makeLink(home, path)
				home = path
				linked = []string{path}
			case "external-skills":
				target := t.TempDir()
				writeScopeFixture(t, filepath.Join(target, "layout", "SKILL.md"), layoutSkill)
				makeLink(target, cursor)
				linked = []string{cursor}
			case "unselected-target":
				makeLink(shared, cursor)
				linked = []string{cursor}
				harnesses = []string{"cursor"}
			}
			before := map[string]string{}
			targets := map[string]string{}
			for _, path := range linked {
				target, err := os.Readlink(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = target
				resolved, err := pathutil.Resolve(path)
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := fsstate.Capture(resolved, fsstate.Options{FollowLinks: false})
				if err != nil {
					t.Fatal(err)
				}
				targets[resolved] = snapshot.State
			}
			runLayoutRoundTrip(t, home, harnesses, func() {
				if kind == "codex-file" || kind == "opencode-parent" || kind == "mcp-file" || kind == "mcp-shared" || kind == "mcp-parent" {
					cfg, err := config.Load(home)
					if err != nil || len(cfg.Servers) != 1 {
						t.Fatalf("native MCP was not imported: %v %+v", err, cfg)
					}
				}
			})
			for path, target := range before {
				actual, err := os.Readlink(path)
				if err != nil || actual != target {
					t.Fatalf("original %s link not restored: %v", kind, err)
				}
			}
			for target, expected := range targets {
				if kind == "home-alias" {
					// Restore archives the canonical configuration under this home.
					// The original native tree is checked by the round trip itself.
					continue
				}
				snapshot, err := fsstate.Capture(target, fsstate.Options{FollowLinks: false})
				if err != nil || snapshot.State != expected {
					t.Fatalf("original target changed: %s: %v", target, err)
				}
			}
		})
	}
}

func TestInitInvalidNativeLinksLeaveOriginalsUntouched(t *testing.T) {
	for _, kind := range []string{"skills-cycle", "skills-broken", "asset-cycle", "mcp-cycle", "mcp-broken"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			writeScopeFixture(t, filepath.Join(home, ".claude", "skills", "layout", "SKILL.md"), layoutSkill)
			var path, target string
			switch kind {
			case "skills-cycle", "skills-broken":
				path = filepath.Join(home, ".cursor", "skills")
				target = path
				if kind == "skills-broken" {
					target = filepath.Join(home, "missing")
				}
			case "asset-cycle":
				target = filepath.Join(home, ".claude", "skills", "layout")
				path = filepath.Join(target, "loop")
			case "mcp-cycle", "mcp-broken":
				path = filepath.Join(home, ".cursor", "mcp.json")
				target = path
				if kind == "mcp-broken" {
					target = filepath.Join(home, "missing.json")
				}
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			before, err := fsstate.Capture(home, fsstate.Options{})
			if err != nil {
				t.Fatal(err)
			}
			inventory, err := bootstrap.Discover(home)
			if err == nil {
				err = checkInitCoverage(inventory, []string{"cursor", "claude-code"})
			}
			if err == nil {
				t.Fatal("invalid native link did not prevent initialization")
			}
			after, err := fsstate.Capture(home, fsstate.Options{})
			if err != nil || after.State != before.State {
				t.Fatalf("invalid native link altered original files: %v", err)
			}
		})
	}
}
