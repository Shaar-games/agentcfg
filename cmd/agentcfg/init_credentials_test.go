package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/broker"
	"agentcfg/internal/syncer"
)

func TestInitCopiesCredentialsAndExpressionsThroughBroker(t *testing.T) {
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
	const secret = "fixture-private-token"
	const expression = "${env:JSM_TOKEN}:%USERPROFILE%:$(literal)"
	args := []string{"-test.run=^TestMCPFixture$", "--", "--api-key", secret, expression}
	definitions := map[string]any{
		"jsm": map[string]any{
			"command": upstream, "args": args,
			"env": map[string]string{
				"AGENTCFG_MCP_FIXTURE": "1", "AGENTCFG_START_LOG": filepath.Join(home, "starts.txt"),
				"AGENTCFG_TEST_API_TOKEN": secret, "AGENTCFG_TEST_EXPRESSION": expression,
			},
		},
		"remote": map[string]any{
			"url":     "http://user:" + secret + "@example.invalid/mcp?token=" + secret,
			"headers": map[string]string{"Authorization": "Bearer " + secret, "X-Expression": expression},
		},
	}
	before, err := json.Marshal(map[string]any{"mcpServers": definitions, "preference": "keep"})
	if err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(home, ".cursor", "mcp.json")
	writeScopeFixture(t, native, string(before))
	binary := cliBinary(t)
	for _, mode := range []string{"--plain", "--yes"} {
		output, err := exec.Command(binary, "init", "--harnesses", "cursor", mode).CombinedOutput()
		if err != nil {
			t.Fatalf("init %s failed: %v\n%s", mode, err, output)
		}
		if !bytes.Contains(output, []byte("Warning: MCP")) || bytes.Contains(output, []byte("Skipped/unsupported")) {
			t.Fatalf("missing nonblocking advisory: %s", output)
		}
		if bytes.Contains(output, []byte(secret)) || bytes.Contains(output, []byte(expression)) || bytes.Contains(output, []byte("\x1b[")) {
			t.Fatal("redirected init output contains private values or ANSI escapes")
		}
		if mode == "--plain" {
			if _, err := os.Stat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) || !bytes.Equal(mustRead(t, native), before) {
				t.Fatal("preview modified configuration")
			}
		}
	}
	var canonical struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(home, ".agentcfg", "mcp.json")), &canonical); err != nil {
		t.Fatal(err)
	}
	for name, original := range definitions {
		// Transport and broker sharing defaults are added; configured values remain literal.
		if canonical.Servers[name]["sharing"] != "shared" {
			t.Fatalf("unexpected sharing default for %s", name)
		}
		delete(canonical.Servers[name], "type")
		delete(canonical.Servers[name], "sharing")
		encoded, _ := json.Marshal(original)
		var expected map[string]any
		_ = json.Unmarshal(encoded, &expected)
		if !reflect.DeepEqual(canonical.Servers[name], expected) {
			t.Fatalf("import changed configured values for %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "starts.txt")); !os.IsNotExist(err) {
		t.Fatal("initialization executed an imported MCP")
	}
	var migrated struct {
		Servers map[string]struct {
			Command string
			Args    []string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mustRead(t, native), &migrated); err != nil {
		t.Fatal(err)
	}
	proxy := migrated.Servers["jsm"]
	if proxy.Command != binary || !strings.HasPrefix(strings.Join(proxy.Args, " "), "connect ") {
		t.Fatal("native MCP was not routed through agentcfg")
	}
	peer := startCLIPeerCommand(t, proxy.Command, home, proxy.Args)
	peer.initialize(t)
	_ = peer.read(t)
	peer.call(t, 2, "configured-values")
	message := peer.read(t)
	var response struct {
		Content struct {
			Token, Expression string
			Args              []string
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(message["result"], &response); err != nil {
		t.Fatal(err)
	}
	if response.Content.Token != secret || response.Content.Expression != expression || !reflect.DeepEqual(response.Content.Args, args) {
		t.Fatal("broker did not pass the exact configured values to the MCP")
	}
	peer.close(t)
	if err := broker.Stop(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	restore, err := syncer.BuildRestorePlan(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, native), before) {
		t.Fatal("restore changed the original configuration")
	}
}
