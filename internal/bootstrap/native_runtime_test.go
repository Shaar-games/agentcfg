package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

func TestCodexDesktopRuntimeIsPreservedWithoutBlockingPortableImports(t *testing.T) {
	home := t.TempDir()
	data, err := os.ReadFile(filepath.Join("testdata", "codex-desktop.toml"))
	if err != nil {
		t.Fatal(err)
	}
	put(t, home, ".codex/config.toml", string(data))
	plan, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Servers) != 1 || plan.Servers[0].Name != "portable" || plan.Servers[0].Server.Cwd != "" {
		t.Fatalf("wrong imported servers: %+v", plan.Servers)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Kind != WarningNativeRuntime || plan.Warnings[0].Server != "node_repl" || plan.Warnings[0].BlocksInit([]string{"codex"}) {
		t.Fatalf("runtime blocks initialization: %+v", plan.Warnings)
	}
	visible, _ := json.Marshal(plan)
	if strings.Contains(string(visible), "private-fixture") {
		t.Fatal("private runtime values leaked into the import inventory")
	}
	if err := plan.Revalidate(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeExclusionRequiresDesktopExecutableAndSessionPipe(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "codex-desktop.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"custom-executable", "other-pipe", "other-name"} {
		t.Run(mutation, func(t *testing.T) {
			raw, _ := json.Marshal(doc["mcp_servers"].(map[string]any)["node_repl"])
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			name := "node_repl"
			switch mutation {
			case "custom-executable":
				fields["command"], _ = json.Marshal("custom-node-repl")
			case "other-name":
				name = "custom"
			case "other-pipe":
				var env map[string]string
				_ = json.Unmarshal(fields["env"], &env)
				env["SKY_CUA_NATIVE_PIPE_DIRECTORY"] = `\\.\pipe\another-service`
				fields["env"], _ = json.Marshal(env)
			}
			if codexDesktopRuntime("codex", name, fields) {
				t.Fatal("ordinary MCP was excluded as an app runtime")
			}
		})
	}
	home := t.TempDir()
	put(t, home, ".codex/config.toml", "[mcp_servers.node_repl]\ncommand='ordinary-mcp-command'\n")
	plan, err := Discover(home)
	if err != nil || len(plan.Servers) != 1 || len(plan.Warnings) != 0 {
		t.Fatalf("ordinary MCP named node_repl was not imported: %v %+v", err, plan)
	}
}

func TestUnsupportedOptionsNameTheOptionWithoutLeakingItsValue(t *testing.T) {
	home := t.TempDir()
	put(t, home, ".codex/config.toml", "[mcp_servers.custom]\ncommand='ordinary-mcp-command'\nenv_vars=['SECRET_VALUE_NOT_FOR_OUTPUT']\nstartup_timeout_sec=120\n")
	plan, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Server != "custom" || !plan.Warnings[0].BlocksInit([]string{"codex"}) {
		t.Fatalf("missing scoped diagnostic: %+v", plan.Warnings)
	}
	message := plan.Warnings[0].Message
	if !strings.Contains(message, `"env_vars"`) || strings.Contains(message, `"startup_timeout_sec"`) || strings.Contains(message, "SECRET_VALUE_NOT_FOR_OUTPUT") || strings.Contains(message, "credentials") {
		t.Fatalf("incorrect option diagnostic: %s", message)
	}
}
