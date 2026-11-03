package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/config"
)

func TestInitialSourcesStagedVerifiedWithoutTouchingNativeSources(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".claude", "skills", "review")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	skillBody := []byte("---\nname: review\ndescription: Review code\n---\n# Keep original.\n")
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), skillBody, 0o600); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{Harnesses: []string{"claude-code", "codex"}, Runtime: config.Runtime{IdleTimeout: "5m", RequestTimeout: "5m"}}
	server := map[string]map[string]any{
		"sample": {"type": "stdio", "command": "node", "args": []string{"server.js"}, "harnesses": []string{"claude-code"}},
	}
	if err := writeInitialSources(home, settings, server, []skillImport{{Name: "review", Source: source}}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(source, "SKILL.md"))
	if err != nil || !bytes.Equal(after, skillBody) {
		t.Fatalf("native source was changed: %q, %v", after, err)
	}
	canonical, err := os.ReadFile(filepath.Join(home, ".agentcfg", "skills", "review", "SKILL.md"))
	if err != nil || !bytes.Equal(canonical, skillBody) {
		t.Fatalf("canonical skill content differs: %q, %v", canonical, err)
	}
	cfg, err := config.Load(home)
	if err != nil || len(cfg.Warnings) != 0 || len(cfg.Skills) != 1 || len(cfg.Servers) != 1 {
		t.Fatalf("invalid generated config: %+v, error=%v", cfg, err)
	}
	if !config.Allows(cfg.Servers["sample"].Harnesses, "claude-code") || config.Allows(cfg.Servers["sample"].Harnesses, "codex") {
		t.Fatal("imported MCP harness restriction not retained")
	}
}

func TestInitWriterRejectsExistingCanonicalWithoutOverwrite(t *testing.T) {
	home := t.TempDir()
	canonical := filepath.Join(home, ".agentcfg")
	if err := os.Mkdir(canonical, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(canonical, "agentcfg.json")
	if err := os.WriteFile(target, []byte("keep-existing-settings"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeInitialSources(home, config.Settings{Harnesses: []string{"codex"}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing settings should not be overwritten: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "keep-existing-settings" {
		t.Fatalf("init overwrote existing config: %s, %v", data, err)
	}
}

func TestInitWriterRejectsInvalidNativeSkillWithoutPublishing(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "native", "review")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("not a skill frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeInitialSources(home, config.Settings{Harnesses: []string{"codex"}}, nil, []skillImport{{Name: "review", Source: source}})
	if err == nil || !strings.Contains(err.Error(), "failed validation") {
		t.Fatalf("invalid canonical skill was published: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
		t.Fatalf("invalid staging became canonical: %v", err)
	}
}

func TestInitWriterRefusesStagedSkillDifferentFromDiscovery(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".claude", "skills", "review")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"),
		[]byte("---\nname: review\ndescription: Test skill\n---\n# Exact content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeInitialSources(home, config.Settings{Harnesses: []string{"claude-code"}},
		nil, []skillImport{{Name: "review", Source: source, TreeSHA256: strings.Repeat("0", 64)}})
	if err == nil || !strings.Contains(err.Error(), "staged bytes differ") {
		t.Fatalf("stale tree proof accepted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
		t.Fatalf("failed verification published canonical sources: %v", err)
	}
}
