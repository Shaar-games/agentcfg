package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/bootstrap"
	"agentcfg/internal/broker"
	"agentcfg/internal/config"
	"agentcfg/internal/syncer"
	toml "github.com/pelletier/go-toml/v2"
)

func TestInitMigratesAllSkillsAndRoutesSelectedHarnessesThroughOneBroker(t *testing.T) {
	binary := cliBinary(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "cache"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = broker.Stop(ctx, home)
	})
	upstream, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	starts := filepath.Join(home, "starts.txt")
	nativeCodex, err := toml.Marshal(map[string]any{"model": "keep", "mcp_servers": map[string]any{"fixture": map[string]any{
		"command": upstream, "args": []string{"-test.run=^TestMCPFixture$"}, "env": map[string]string{"AGENTCFG_MCP_FIXTURE": "1", "AGENTCFG_START_LOG": starts},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	writeScopeFixture(t, filepath.Join(home, ".codex", "config.toml"), string(nativeCodex))
	nativeCursor := `{"theme":"keep","mcpServers":{"remote":{"url":"https://example.invalid/mcp"}}}`
	writeScopeFixture(t, filepath.Join(home, ".cursor", "mcp.json"), nativeCursor)
	sources := map[string]string{
		".agents/skills/shared": "shared", ".claude/skills/review": "review", ".cursor/skills/review": "review",
		".codex/skills/current": "current", ".opencode/skills/legacy": "legacy", ".cursor/skills/restricted": "restricted",
	}
	for path, name := range sources {
		metadata := ""
		if name == "restricted" {
			metadata = "metadata:\n  harness: none\n"
		}
		writeScopeFixture(t, filepath.Join(home, filepath.FromSlash(path), "SKILL.md"), "---\nname: "+name+"\ndescription: Inert import fixture\n"+metadata+"---\nOriginal instructions.\n")
		writeScopeFixture(t, filepath.Join(home, filepath.FromSlash(path), "references", "rules.txt"), "Keep this asset.\n")
	}
	before := projectSnapshot(t, home)
	command := exec.Command(binary, "init", "--harnesses", "codex,cursor", "--yes")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, output)
	}
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 2 || len(cfg.Skills) != 5 {
		t.Fatalf("incomplete canonical inventory: %+v", cfg)
	}
	for _, server := range cfg.Servers {
		if len(server.Harnesses) != 0 {
			t.Fatal("native provenance became a canonical availability restriction")
		}
	}
	if _, err := os.Stat(starts); !os.IsNotExist(err) {
		t.Fatal("init executed a discovered MCP")
	}
	for path, name := range sources {
		if strings.HasPrefix(path, ".agents/") || strings.HasPrefix(path, ".claude/") || strings.HasPrefix(path, ".opencode/") || name == "restricted" {
			if _, err := os.Lstat(filepath.Join(home, filepath.FromSlash(path))); !os.IsNotExist(err) {
				t.Fatalf("native source was not moved: %s", path)
			}
		}
	}
	for _, harness := range []string{"codex", "cursor"} {
		for _, name := range []string{"shared", "review", "current", "legacy"} {
			native := filepath.Join(home, "."+harness, "skills", name)
			info, err := os.Lstat(native)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("expected a physical copy at %s: %v", native, err)
			}
			if !bytes.Equal(mustRead(t, filepath.Join(native, "SKILL.md")), mustRead(t, filepath.Join(home, ".agentcfg", "skills", name, "SKILL.md"))) {
				t.Fatal("skill instructions changed")
			}
			if string(mustRead(t, filepath.Join(native, "references", "rules.txt"))) != "Keep this asset.\n" {
				t.Fatal("skill asset lost")
			}
		}
	}
	var cursor map[string]any
	if err := json.Unmarshal(mustRead(t, filepath.Join(home, ".cursor", "mcp.json")), &cursor); err != nil {
		t.Fatal(err)
	}
	var codex map[string]any
	if err := toml.Unmarshal(mustRead(t, filepath.Join(home, ".codex", "config.toml")), &codex); err != nil {
		t.Fatal(err)
	}
	if cursor["theme"] != "keep" || codex["model"] != "keep" {
		t.Fatal("unrelated harness settings changed")
	}
	entries := []map[string]any{cursor["mcpServers"].(map[string]any), codex["mcp_servers"].(map[string]any)}
	var peers []*cliPeer
	for _, servers := range entries {
		for _, raw := range servers {
			encoded, _ := json.Marshal(raw)
			var proxy struct {
				Command string
				Args    []string
			}
			if err := json.Unmarshal(encoded, &proxy); err != nil {
				t.Fatal(err)
			}
			if proxy.Command != binary || len(proxy.Args) == 0 || proxy.Args[0] != "connect" {
				t.Fatal("native MCP bypasses agentcfg")
			}
		}
		encoded, _ := json.Marshal(servers["fixture"])
		var proxy struct {
			Command string
			Args    []string
		}
		_ = json.Unmarshal(encoded, &proxy)
		peers = append(peers, startCLIPeerCommand(t, proxy.Command, home, proxy.Args))
	}
	for _, peer := range peers {
		peer.initialize(t)
		_ = peer.read(t)
		peer.call(t, 2, "init-broker-check")
	}
	if first, second := resultPID(t, peers[0].read(t), "init-broker-check"), resultPID(t, peers[1].read(t), "init-broker-check"); first != second {
		t.Fatalf("init generated separate upstreams: %d != %d", first, second)
	}
	for _, peer := range peers {
		peer.close(t)
	}
	if err := broker.Stop(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Scope: "user", Binary: binary, SkillMode: "copy"})
	if err != nil || len(plan.Changes) != 0 {
		t.Fatalf("post-init sync is not idempotent: %v %+v", err, plan)
	}
	restore, err := syncer.BuildRestorePlan(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(home, ".codex", "config.toml")), nativeCodex) || string(mustRead(t, filepath.Join(home, ".cursor", "mcp.json"))) != nativeCursor {
		t.Fatal("restore changed the original MCP files")
	}
	after := projectSnapshot(t, home)
	for path := range before {
		if strings.HasPrefix(path, "cache") {
			continue
		}
		if after[path] != before[path] {
			t.Fatalf("restore did not recover original native content: %s", path)
		}
	}
}

func TestFailedInitArchivesCanonicalAndKeepsNativeConfiguration(t *testing.T) {
	home := t.TempDir()
	native := filepath.Join(home, ".cursor", "mcp.json")
	before := `{"mcpServers":{"collision":{"command":"manual-command"}}}`
	writeScopeFixture(t, native, before)
	proposal := initProposal{DetectedHarnesses: []string{"cursor"}, Servers: map[string]map[string]any{"collision": {"command": "canonical-command"}}}
	err := runInitWizard(context.Background(), home, proposal, initOptions{Yes: true, Binary: cliBinary(t)}, nil, &bytes.Buffer{})
	if err == nil {
		t.Fatal("unproved native collision was overwritten")
	}
	if string(mustRead(t, native)) != before {
		t.Fatal("failed initialization changed native MCP")
	}
	if _, err := os.Stat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
		t.Fatal("failed init left an apparently initialized setup")
	}
	archives, err := filepath.Glob(filepath.Join(home, ".agentcfg-restored-*", "mcp.json"))
	if err != nil || len(archives) != 1 || !bytes.Contains(mustRead(t, archives[0]), []byte("canonical-command")) {
		t.Fatal("failed init lost imported sources")
	}
}

func TestInitRejectsIncompleteImportBeforeWriting(t *testing.T) {
	for _, invalid := range []string{"mcp", "skill", "conflict"} {
		t.Run(invalid, func(t *testing.T) {
			home := t.TempDir()
			switch invalid {
			case "mcp":
				writeScopeFixture(t, filepath.Join(home, ".cursor", "mcp.json"), `{"mcpServers":{"custom":{"command":"inert","unsupportedOption":true}}}`)
			case "skill":
				writeScopeFixture(t, filepath.Join(home, ".claude", "skills", "review", "SKILL.md"), "Not a portable skill.")
			case "conflict":
				writeScopeFixture(t, filepath.Join(home, ".cursor", "mcp.json"), `{"mcpServers":{"same":{"command":"one"}}}`)
				writeScopeFixture(t, filepath.Join(home, ".codex", "config.toml"), "[mcp_servers.same]\ncommand='two'\n")
			}
			before := projectSnapshot(t, home)
			inventory, err := bootstrap.Discover(home)
			if err != nil {
				t.Fatal(err)
			}
			proposal, err := proposalFromNative(inventory)
			if err != nil {
				t.Fatal(err)
			}
			err = runInitWizard(context.Background(), home, proposal, initOptions{Yes: true, Harnesses: []string{"codex", "cursor"}}, nil, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "init --plain") {
				t.Fatalf("incomplete init did not report the source: %v", err)
			}
			if !sameProjectSnapshot(before, projectSnapshot(t, home)) {
				t.Fatal("incomplete import changed native or canonical data")
			}
		})
	}
}

func TestInitMigratesLegacyOpenCodeConfiguration(t *testing.T) {
	home := t.TempDir()
	native := filepath.Join(home, ".opencode", "opencode.jsonc")
	before := "// keep this comment\n" + `{"theme":"keep","mcp":{"local":{"type":"local","command":["inert","arg"]}}}`
	writeScopeFixture(t, native, before)
	inventory, err := bootstrap.Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := proposalFromNative(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if err := runInitWizard(context.Background(), home, proposal, initOptions{Yes: true, Harnesses: []string{"opencode"}, Binary: cliBinary(t)}, nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	after := mustRead(t, native)
	if bytes.Contains(after, []byte(`"inert"`)) || !bytes.Contains(after, []byte(`"connect"`)) || !bytes.Contains(after, []byte("keep this comment")) {
		t.Fatal("legacy MCP was not replaced conservatively")
	}
	restore, err := syncer.BuildRestorePlan(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, native)) != before {
		t.Fatal("legacy original was not restored")
	}
}
