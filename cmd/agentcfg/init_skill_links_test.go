package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"agentcfg/internal/bootstrap"
	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"
	"agentcfg/internal/syncer"
)

type nativeSkillLink func(t *testing.T, target, path string)

func TestInitMigratesAndRestoresSkillSymlinks(t *testing.T) {
	for _, relative := range []bool{false, true} {
		name := "absolute"
		if relative {
			name = "relative"
		}
		t.Run(name, func(t *testing.T) {
			testInitSkillLinks(t, func(t *testing.T, target, path string) {
				if relative {
					var err error
					target, err = filepath.Rel(filepath.Dir(path), target)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("directory symlinks unavailable: %v", err)
				}
			})
		})
	}
}

func testInitSkillLinks(t *testing.T, link nativeSkillLink) {
	t.Helper()
	for _, layout := range []string{"pack", "shared", "claude-chain", "cursor-chain", "identical-sources"} {
		t.Run(layout, func(t *testing.T) {
			home := t.TempDir()
			contents := "---\nname: twg\ndescription: Linked import fixture.\n---\nOriginal instructions.\n"
			pack := filepath.Join(home, "packs", "twg")
			shared := filepath.Join(home, ".agents", "skills", "twg")
			if layout != "pack" && layout != "identical-sources" {
				pack = shared
			}
			writeScopeFixture(t, filepath.Join(pack, "SKILL.md"), contents)
			writeScopeFixture(t, filepath.Join(pack, "assets", "rules.txt"), "Original asset.\n")
			if layout == "identical-sources" {
				writeScopeFixture(t, filepath.Join(shared, "SKILL.md"), contents)
				writeScopeFixture(t, filepath.Join(shared, "assets", "rules.txt"), "Original asset.\n")
			}
			packBefore, err := os.Stat(pack)
			if err != nil {
				t.Fatal(err)
			}
			claude := filepath.Join(home, ".claude", "skills", "twg")
			cursor := filepath.Join(home, ".cursor", "skills", "twg")
			for _, path := range []string{claude, cursor} {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			switch layout {
			case "claude-chain":
				link(t, pack, cursor)
				link(t, cursor, claude)
			case "cursor-chain":
				link(t, pack, claude)
				link(t, claude, cursor)
			default:
				link(t, pack, claude)
				link(t, pack, cursor)
			}
			links := map[string]string{}
			for _, path := range []string{claude, cursor} {
				links[path], err = os.Readlink(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			inventory, err := bootstrap.Discover(home)
			if err != nil || len(inventory.Skills) != 1 || len(inventory.Warnings) != 0 {
				t.Fatalf("linked discovery failed: %v %+v", err, inventory)
			}
			proposal, err := proposalFromNative(inventory)
			if err != nil {
				t.Fatal(err)
			}
			// Cursor stays unselected: its imported link must be retired too.
			if err := runInitWizard(context.Background(), home, proposal, initOptions{Yes: true, Harnesses: []string{"claude-code", "codex"}}, nil, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{filepath.Join(home, ".agentcfg", "skills", "twg"), claude, filepath.Join(home, ".codex", "skills", "twg")} {
				info, err := os.Lstat(path)
				if err != nil || !info.IsDir() || pathutil.IsRedirected(info) || string(mustRead(t, filepath.Join(path, "SKILL.md"))) != contents || string(mustRead(t, filepath.Join(path, "assets", "rules.txt"))) != "Original asset.\n" {
					t.Fatalf("expected a complete physical skill copy at %s: %v", path, err)
				}
			}
			if _, err := os.Lstat(cursor); !os.IsNotExist(err) {
				t.Fatal("unselected imported native link was not retired")
			}
			if layout == "pack" || layout == "identical-sources" {
				after, err := os.Stat(pack)
				if err != nil || !os.SameFile(packBefore, after) || string(mustRead(t, filepath.Join(pack, "SKILL.md"))) != contents {
					t.Fatal("replacing a native link modified its independent target")
				}
			} else if _, err := os.Lstat(shared); !os.IsNotExist(err) {
				t.Fatal("shared import was not moved into canonical sources")
			}
			cfg, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Scope: "user", SkillMode: "copy"})
			if err != nil || len(plan.Changes) != 0 {
				t.Fatalf("post-init sync is not idempotent: %v", err)
			}
			restore, err := syncer.BuildRestorePlan(home)
			if err != nil {
				t.Fatal(err)
			}
			if err := restore.Apply(); err != nil {
				t.Fatal(err)
			}
			for path, target := range links {
				actual, err := os.Readlink(path)
				if err != nil || actual != target || string(mustRead(t, filepath.Join(path, "SKILL.md"))) != contents {
					t.Fatalf("original link or its target was not restored at %s: %v", path, err)
				}
			}
		})
	}
}
