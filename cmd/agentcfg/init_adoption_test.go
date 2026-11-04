package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/bootstrap"
	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"
	"agentcfg/internal/syncer"
)

func TestInitNativeAdoptionRequiresExplicitProvenProofs(t *testing.T) {
	home := t.TempDir()
	native := filepath.Join(home, ".claude.json")
	original := []byte(`{"unrelated":{"keep":true},"mcpServers":{"local":{"command":"node","args":["server.js"]}}}`)
	if err := os.WriteFile(native, original, 0o600); err != nil {
		t.Fatal(err)
	}
	skillSource := filepath.Join(home, ".claude", "skills", "review")
	if err := os.MkdirAll(skillSource, 0o700); err != nil {
		t.Fatal(err)
	}
	skillBytes := []byte("---\nname: review\ndescription: Inspect files\n---\n# Content is unchanged\n")
	if err := os.WriteFile(filepath.Join(skillSource, "SKILL.md"), skillBytes, 0o600); err != nil {
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
	if len(proposal.Servers) != 1 || len(proposal.Skills) != 1 {
		t.Fatalf("native sources not discovered: %+v", proposal)
	}
	settings := config.Settings{Harnesses: []string{"claude-code"}, Runtime: config.Runtime{IdleTimeout: "5m", RequestTimeout: "5m"}}
	if err := writeInitialSources(home, settings, proposal.Servers, proposal.Skills); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(native)
	if err != nil || !bytes.Equal(before, original) {
		t.Fatalf("canonical initialization changed native MCP: %v", err)
	}
	mcp, skills, err := initAdoptionProofs(inventory, proposal.Servers, proposal.Skills, settings.Harnesses)
	if err != nil {
		t.Fatal(err)
	}
	// Scanner origins retain their native spelling, while adoption proofs use
	// the physical target path. These can differ on macOS (/var aliases) and
	// Windows (NTFS short names) without changing the adopted native entry.
	physicalSkill, err := pathutil.Resolve(skillSource)
	if err != nil {
		t.Fatal(err)
	}
	physicalNative, err := pathutil.Resolve(native)
	if err != nil {
		t.Fatal(err)
	}
	if len(mcp) != 1 || len(skills) != 1 || mcp[0].Harness != "claude-code" ||
		skills[0].Path != physicalSkill || mcp[0].Path != physicalNative {
		t.Fatalf("unexpected adoption proof scope: %+v %+v", mcp, skills)
	}
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(home, "agentcfg.exe")
	_, err = syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Scope: "user", Binary: binary})
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("unowned native entry was not protected by default: %v", err)
	}
	opts := syncer.Options{
		Root: home, Home: home, Scope: "user", Binary: binary, AdoptExisting: true,
		AdoptMCP: mcp, AdoptSkills: skills,
	}
	plan, err := syncer.BuildPlan(cfg, opts)
	if err != nil {
		t.Fatalf("verified explicit adoption refused: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("no native changes planned")
	}
	// Even after a reviewable plan was generated, no native file is modified
	// until the separate confirmation authorizes Apply.
	before, err = os.ReadFile(native)
	if err != nil || !bytes.Equal(before, original) {
		t.Fatal("BuildPlan wrote native configuration")
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(native)
	if err != nil || !bytes.Contains(after, []byte("agentcfg.exe")) ||
		!bytes.Contains(after, []byte("unrelated")) || bytes.Contains(after, []byte("server.js")) {
		t.Fatalf("adopted MCP native config invalid: %v, %s", err, after)
	}
	canonical, err := os.ReadFile(filepath.Join(home, ".agentcfg", "skills", "review", "SKILL.md"))
	if err != nil || !bytes.Equal(canonical, skillBytes) {
		t.Fatalf("canonical source SKILL.md changed: %v", err)
	}
}

func TestInitAdoptionProofsRejectChangedScannerSource(t *testing.T) {
	home := t.TempDir()
	native := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(native, []byte(`{"mcpServers":{"local":{"command":"node"}}}`), 0o600); err != nil {
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
	if err := os.WriteFile(native, []byte(`{"mcpServers":{"local":{"command":"changed"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := initAdoptionProofs(inventory, proposal.Servers, proposal.Skills, []string{"claude-code"}); err == nil {
		t.Fatal("stale scanner MCP proof accepted")
	}
}

func TestInitCodexTOMLAdoptionReplacesProvenNativeEntry(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(home, ".codex", "config.toml")
	if err := os.WriteFile(native, []byte("[mcp_servers.local]\ncommand = 'node'\n"), 0o600); err != nil {
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
	if err := writeInitialSources(home, config.Settings{Harnesses: []string{"codex"}}, proposal.Servers, nil); err != nil {
		t.Fatal(err)
	}
	mcp, skills, err := initAdoptionProofs(inventory, proposal.Servers, nil, []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Scope: "user", Binary: filepath.Join(home, "agentcfg.exe"),
		AdoptExisting: true, AdoptMCP: mcp, AdoptSkills: skills})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(mustRead(t, native), []byte("agentcfg.exe")) {
		t.Fatal("Codex did not receive the broker proxy")
	}
}

func TestInitYesWithCodexTOMLMigratesAndRestoresNative(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(home, ".codex", "config.toml")
	original := []byte("[mcp_servers.local]\ncommand = 'node'\n")
	if err := os.WriteFile(native, original, 0o600); err != nil {
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
	var output bytes.Buffer
	err = runInitWizard(context.Background(), home, proposal, initOptions{Yes: true, Binary: cliBinary(t)}, nil, &output)
	if err != nil {
		t.Fatalf("canonical import of native TOML must remain allowed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agentcfg", "mcp.json")); err != nil {
		t.Fatalf("canonical import not created: %v", err)
	}
	if after, err := os.ReadFile(native); err != nil || bytes.Equal(after, original) {
		t.Fatalf("native Codex TOML was not migrated: %s, %v", after, err)
	}
	restore, err := syncer.BuildRestorePlan(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, native), original) {
		t.Fatal("restore did not recover original Codex TOML")
	}
}

func TestInitAdoptionThroughSymlinkedParentKeepsNativeOrigins(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "real")
	home := filepath.Join(parent, "user")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Skipf("cannot create system-style path alias on this filesystem: %v", err)
	}
	aliasHome := filepath.Join(alias, "user")
	path := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"local":{"command":"node"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	inventory, err := bootstrap.Discover(aliasHome)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := proposalFromNative(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Servers) != 1 || len(inventory.Servers) != 1 ||
		inventory.Servers[0].Origins[0].Source != filepath.Join(aliasHome, ".cursor", "mcp.json") {
		t.Fatalf("native aliases became an unreviewed different source: %+v", inventory.Servers)
	}
	if err := writeInitialSources(aliasHome, config.Settings{Harnesses: []string{"cursor"}}, proposal.Servers, nil); err != nil {
		t.Fatal(err)
	}
	mcp, _, err := initAdoptionProofs(inventory, proposal.Servers, nil, []string{"cursor"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(aliasHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.BuildPlan(cfg, syncer.Options{
		Root: aliasHome, Home: aliasHome, Scope: "user", Binary: filepath.Join(aliasHome, "agentcfg"),
		AdoptExisting: true, AdoptMCP: mcp,
	}); err != nil {
		t.Fatalf("safe native alias was incorrectly rejected: %v; scanner=%+v; canonical=%+v", err, inventory.Servers[0].Server, cfg.Servers["local"])
	}
}
