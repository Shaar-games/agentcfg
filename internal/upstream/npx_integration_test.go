package upstream

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/config"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This opt-in integration uses the real npm launcher and official filesystem
// MCP, with private files, npm configuration, cache and home directories.
// It exercises Start directly so the launcher can be checked without a broker
// socket. TestCLINPXFilesystem additionally checks the CLI and broker path.
func TestNPXFilesystem(t *testing.T) {
	if os.Getenv("AGENTCFG_TEST_NPX") != "1" {
		t.Skip("set AGENTCFG_TEST_NPX=1 to exercise the real npm filesystem MCP")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, "node", "-p", "process.versions.node").Output()
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
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed files with spaces")
	privateHome := filepath.Join(root, "home")
	for _, directory := range []string{allowed, privateHome, filepath.Join(root, "appdata"), filepath.Join(root, "localappdata")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(allowed, "message é with spaces.txt")
	const contents = "agentcfg real npm MCP round trip\n"
	if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside-allowed.txt")
	if err := os.WriteFile(outside, []byte("must not be readable through MCP"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"npmrc", "global-npmrc"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(root, "npx-stderr.log")
	diagnostics, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--yes", "--package=@modelcontextprotocol/server-filesystem@2.2.0", "mcp-server-filesystem", allowed}
	transport, err := Start(ctx, config.Server{
		Type: "stdio", Command: "npx", Args: args, Cwd: root,
		Env: map[string]string{
			"HOME": privateHome, "USERPROFILE": privateHome,
			"APPDATA": filepath.Join(root, "appdata"), "LOCALAPPDATA": filepath.Join(root, "localappdata"),
			"npm_config_cache":           filepath.Join(root, "npm-cache"),
			"npm_config_userconfig":      filepath.Join(root, "npmrc"),
			"npm_config_globalconfig":    filepath.Join(root, "global-npmrc"),
			"npm_config_update_notifier": "false", "npm_config_audit": "false",
			"npm_config_fund": "false", "npm_config_ignore_scripts": "true",
			"NPM_TOKEN": "", "NODE_AUTH_TOKEN": "", "NODE_OPTIONS": "",
		},
	}, diagnostics)
	t.Cleanup(func() {
		if transport != nil {
			_ = transport.Close()
		}
		_ = diagnostics.Close()
		if t.Failed() {
			if data, err := os.ReadFile(logPath); err == nil {
				if len(data) > 8192 {
					data = data[len(data)-8192:]
				}
				t.Logf("npx stderr: %s", data)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	verifyGone := watchHelperProcess(t, transport.PID())
	client := mcp.NewClient(&mcp.Implementation{Name: "agentcfg-npx-test", Version: "1"}, nil)
	// The SDK negotiates discovery, with its normal legacy initialize fallback.
	// The adapter still passes every message through Transport.Read and Write.
	session, err := client.Connect(ctx, npxSDKTransport{transport}, nil)
	if err != nil {
		t.Fatalf("connect real filesystem MCP through npx: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("real filesystem tools/list: %v", err)
	}
	found := false
	for _, tool := range tools.Tools {
		found = found || tool.Name == "read_text_file"
	}
	if !found {
		t.Fatal("real filesystem MCP did not advertise read_text_file")
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "read_text_file", Arguments: map[string]any{"path": file},
	})
	if err != nil || result.IsError {
		t.Fatalf("read_text_file: result=%v err=%v", result, err)
	}
	var returned strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			returned.WriteString(text.Text)
		}
	}
	if !strings.Contains(returned.String(), contents) {
		t.Fatalf("real filesystem tool changed contents: %q", returned.String())
	}
	denied, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "read_text_file", Arguments: map[string]any{"path": outside},
	})
	if err != nil || !denied.IsError {
		t.Fatalf("outside-directory read was not a tool error: result=%v err=%v", denied, err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close real npx MCP: %v", err)
	}
	verifyGone()
	t.Logf("@modelcontextprotocol/server-filesystem@2.2.0 via npx, Node %s, protocol %s: %d tools, Unicode/spaced file contents=%q, outside-directory denial and process shutdown verified",
		strings.TrimSpace(string(version)), session.InitializeResult().ProtocolVersion, len(tools.Tools), contents)
}

type npxSDKTransport struct{ transport Transport }

func (t npxSDKTransport) Connect(context.Context) (mcp.Connection, error) {
	return npxSDKConnection{t.transport}, nil
}

type npxSDKConnection struct{ Transport }

func (c npxSDKConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	message, err := c.Transport.Read()
	if err != nil {
		return nil, err
	}
	return decodeSDKMessage(message)
}

func (c npxSDKConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := encodeSDKMessage(message)
	if err != nil {
		return err
	}
	return c.Transport.Write(encoded)
}

func (c npxSDKConnection) SessionID() string { return "" }
