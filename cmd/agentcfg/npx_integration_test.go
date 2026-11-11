package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/broker"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This opt-in test downloads and runs the official filesystem MCP through
// the actual npx launcher, CLI proxy and broker. The normal suite is offline.
// Every canonical config, npm cache and exposed file belongs to a fixture.
func TestCLINPXFilesystem(t *testing.T) {
	if os.Getenv("AGENTCFG_TEST_NPX") != "1" {
		t.Skip("set AGENTCFG_TEST_NPX=1 to exercise the real npm filesystem MCP")
	}
	version, err := exec.Command("node", "-p", "process.versions.node").Output()
	if err != nil {
		t.Fatalf("real npx integration requires Node.js >=24: %v", err)
	}
	major, err := strconv.Atoi(strings.Split(strings.TrimSpace(string(version)), ".")[0])
	if err != nil || major < 24 {
		t.Fatalf("filesystem MCP 2.2.0 requires Node.js >=24, got %q", version)
	}
	if _, err := exec.LookPath("npx"); err != nil {
		t.Fatalf("real npx integration requires npx on PATH: %v", err)
	}
	binary := cliBinary(t)
	cache := t.TempDir()
	for _, sharing := range []string{"shared", "isolated"} {
		t.Run(sharing, func(t *testing.T) {
			root := integrationRoot(t, sharing, "10s")
			allowed := filepath.Join(root, "allowed files with spaces")
			if err := os.MkdirAll(allowed, 0o700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(allowed, "message-é.txt")
			const contents = "agentcfg real npm MCP round trip\n"
			if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(root, "outside-allowed.txt")
			if err := os.WriteFile(outside, []byte("must not be readable through MCP"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"npmrc", "global-npmrc"} {
				if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeNPXFixtureJSON(t, root, "mcp.json", map[string]any{
				"mcpServers": map[string]any{"fixture": map[string]any{
					"command": "npx", "sharing": sharing,
					"args": []string{"--yes", "--package=@modelcontextprotocol/server-filesystem@2.2.0", "mcp-server-filesystem", allowed},
					"env": map[string]string{
						"npm_config_cache": cache, "npm_config_update_notifier": "false",
						"npm_config_userconfig":   filepath.Join(root, "npmrc"),
						"npm_config_globalconfig": filepath.Join(root, "global-npmrc"),
						"npm_config_audit":        "false", "npm_config_fund": "false",
						"npm_config_ignore_scripts": "true",
						"NPM_TOKEN":                 "", "NODE_AUTH_TOKEN": "", "NODE_OPTIONS": "",
					},
				}},
			})
			writeNPXFixtureJSON(t, root, "agentcfg.json", map[string]any{
				"harnesses": []string{"cursor", "codex"},
				"runtime":   map[string]string{"idleTimeout": "10s", "requestTimeout": "90s"},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			connect := func(harness string) *mcp.ClientSession {
				t.Helper()
				cmd := exec.Command(binary, "connect", "--server", "fixture", "--harness", harness)
				cmd.Dir = root
				cmd.Env = os.Environ() // integrationRoot already isolates HOME/USERPROFILE.
				var diagnostics safeBuffer
				cmd.Stderr = &diagnostics
				client := mcp.NewClient(&mcp.Implementation{Name: "agentcfg-npx-test", Version: "1"}, nil)
				session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
				if err != nil {
					t.Fatalf("connect real npx MCP for %s: %v; stderr: %s", harness, err, diagnostics.String())
				}
				t.Cleanup(func() { _ = session.Close() })
				return session
			}
			first, second := connect("cursor"), connect("codex")
			for i, session := range []*mcp.ClientSession{first, second} {
				tools, err := session.ListTools(ctx, nil)
				if err != nil {
					t.Fatalf("client %d tools/list: %v", i, err)
				}
				found := false
				for _, tool := range tools.Tools {
					found = found || tool.Name == "read_text_file"
				}
				if !found {
					t.Fatal("real filesystem server did not advertise read_text_file")
				}
				result, err := session.CallTool(ctx, &mcp.CallToolParams{
					Name: "read_text_file", Arguments: map[string]any{"path": file},
				})
				if err != nil || result.IsError || !strings.Contains(npxResultText(result), contents) {
					t.Fatalf("client %d real tool call: result=%v err=%v", i, result, err)
				}
			}
			status, err := broker.GetStatus(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			wantBackends := 1
			if sharing == "isolated" {
				wantBackends = 2
			}
			if len(status.Backends) != wantBackends {
				t.Fatalf("%s real npx process count: %+v", sharing, status)
			}
			denied, err := second.CallTool(ctx, &mcp.CallToolParams{
				Name: "read_text_file", Arguments: map[string]any{"path": outside},
			})
			if err != nil || !denied.IsError {
				t.Fatalf("server's file access error did not survive proxy: result=%v err=%v", denied, err)
			}
			if err := first.Close(); err != nil {
				t.Fatalf("close first proxy: %v", err)
			}
			result, err := second.CallTool(ctx, &mcp.CallToolParams{
				Name: "read_text_file", Arguments: map[string]any{"path": file},
			})
			if err != nil || result.IsError || !strings.Contains(npxResultText(result), contents) {
				t.Fatalf("second harness failed after first disconnected: result=%v err=%v", result, err)
			}
			t.Logf("@modelcontextprotocol/server-filesystem@2.2.0 via npx, Node %s: %s, %d backend(s), two clients, tools/list, Unicode/spaced path, tool failure and surviving client verified", strings.TrimSpace(string(version)), sharing, wantBackends)
		})
	}
}

func writeNPXFixtureJSON(t *testing.T, root, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".agentcfg", name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func npxResultText(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var text strings.Builder
	for _, content := range result.Content {
		if part, ok := content.(*mcp.TextContent); ok {
			fmt.Fprint(&text, part.Text)
		}
	}
	return text.String()
}
