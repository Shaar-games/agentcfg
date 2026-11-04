package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/bootstrap"
	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"
	"agentcfg/internal/syncer"
)

func TestInitPreservesCodexDesktopRuntimeAndDoesNotInventCwd(t *testing.T) {
	home := t.TempDir()
	native := filepath.Join(home, ".codex", "config.toml")
	before, err := os.ReadFile(filepath.Join("..", "..", "internal", "bootstrap", "testdata", "codex-desktop.toml"))
	if err != nil {
		t.Fatal(err)
	}
	writeScopeFixture(t, native, string(before))
	inventory, err := bootstrap.Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := proposalFromNative(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.NativeServers) != 1 || len(proposal.Warnings) != 0 {
		t.Fatalf("runtime was reported as an import failure: %+v", proposal)
	}
	var output bytes.Buffer
	if err := runInitWizard(context.Background(), home, proposal, initOptions{Yes: true, Harnesses: []string{"codex", "cursor"}, Binary: cliBinary(t)}, nil, &output); err != nil {
		t.Fatal(err)
	}
	after := mustRead(t, native)
	if !bytes.HasPrefix(after, bytes.Split(before, []byte("[mcp_servers.portable]"))[0]) {
		t.Fatal("desktop settings or runtime block were rewritten")
	}
	var canonical struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(home, ".agentcfg", "mcp.json")), &canonical); err != nil {
		t.Fatal(err)
	}
	if len(canonical.Servers) != 1 || canonical.Servers["portable"] == nil {
		t.Fatal("desktop runtime was imported or user MCP was lost")
	}
	if _, exists := canonical.Servers["portable"]["cwd"]; exists {
		t.Fatal("init invented a cwd in canonical configuration")
	}
	if !bytes.Contains(after, []byte("connect")) || bytes.Contains(mustRead(t, filepath.Join(home, ".cursor", "mcp.json")), []byte("node_repl")) {
		t.Fatal("user MCP routing or desktop isolation is incorrect")
	}
	cfg, err := config.Load(home)
	if err != nil || cfg.Servers["portable"].Cwd != "" {
		t.Fatalf("loader forced an implicit cwd: %v %+v", err, cfg)
	}
	restore, err := syncer.BuildRestorePlan(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, native), before) {
		t.Fatal("restore did not recover the exact Codex configuration")
	}
}

func TestNativeCwdIsImportedOnlyWhenExplicit(t *testing.T) {
	home := t.TempDir()
	directory := filepath.Join(home, "workspace")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"implicit": map[string]any{"command": "inert-command"},
		"explicit": map[string]any{"command": "inert-command", "cwd": directory},
	}})
	writeScopeFixture(t, filepath.Join(home, ".cursor", "mcp.json"), string(raw))
	inventory, err := bootstrap.Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := proposalFromNative(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := proposal.Servers["implicit"]["cwd"]; exists {
		t.Fatal("implicit cwd became canonical source data")
	}
	explicit, _ := pathutil.Resolve(directory)
	got, _ := pathutil.Resolve(proposal.Servers["explicit"]["cwd"].(string))
	if got != explicit {
		t.Fatal("explicit native cwd was lost")
	}
}

func TestCLICwdFollowsEachCallerAndPartitionsSharedProcesses(t *testing.T) {
	binary := cliBinary(t)
	home := integrationRoot(t, "shared", "5s")
	firstDir, secondDir, fixedDir := t.TempDir(), t.TempDir(), t.TempDir()
	start := func(harness, directory string) *cliPeer {
		return startCLIPeerCommand(t, binary, home, []string{"connect", "--server", "fixture", "--harness", harness}, directory)
	}
	read := func(peer *cliPeer, directory string) int {
		peer.initialize(t)
		_ = peer.read(t)
		peer.call(t, 2, "working-directory")
		message := peer.read(t)
		var response struct {
			Result struct {
				Structured struct {
					Cwd string `json:"cwd"`
				} `json:"structuredContent"`
			} `json:"result"`
		}
		encoded, _ := json.Marshal(message)
		if err := json.Unmarshal(encoded, &response); err != nil {
			t.Fatal(err)
		}
		expected, err := pathutil.Resolve(directory)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := pathutil.Resolve(response.Result.Structured.Cwd)
		if err != nil || actual != expected {
			t.Fatalf("MCP cwd = %q, expected caller cwd %q: %v", response.Result.Structured.Cwd, expected, err)
		}
		return resultPID(t, message, "working-directory")
	}
	a, b, c := start("codex", firstDir), start("cursor", firstDir), start("cursor", secondDir)
	pidA, pidB, pidC := read(a, firstDir), read(b, firstDir), read(c, secondDir)
	if pidA != pidB || pidA == pidC {
		t.Fatalf("shared MCP contexts mixed: first=%d same-dir=%d other-dir=%d", pidA, pidB, pidC)
	}
	for _, peer := range []*cliPeer{a, b, c} {
		peer.close(t)
	}
	canonical := filepath.Join(home, ".agentcfg", "mcp.json")
	var doc map[string]any
	if err := json.Unmarshal(mustRead(t, canonical), &doc); err != nil {
		t.Fatal(err)
	}
	doc["mcpServers"].(map[string]any)["fixture"].(map[string]any)["cwd"] = fixedDir
	encoded, _ := json.Marshal(doc)
	writeScopeFixture(t, canonical, string(encoded))
	d, e := start("codex", firstDir), start("cursor", secondDir)
	if read(d, fixedDir) != read(e, fixedDir) {
		t.Fatal("explicit cwd did not allow sharing across caller directories")
	}
}

func TestPlainInitExplainsPreservedRuntimeWithoutPrivateValues(t *testing.T) {
	home := t.TempDir()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "bootstrap", "testdata", "codex-desktop.toml"))
	if err != nil {
		t.Fatal(err)
	}
	writeScopeFixture(t, filepath.Join(home, ".codex", "config.toml"), string(data))
	inventory, err := bootstrap.Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := proposalFromNative(inventory)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runInitWizard(context.Background(), home, proposal, initOptions{Plain: true}, nil, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Kept native: codex/node_repl") || strings.Contains(output.String(), "private-fixture") {
		t.Fatalf("incorrect runtime diagnostic: %s", output.String())
	}
}
