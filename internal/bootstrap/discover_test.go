package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"agentcfg/internal/config"
)

func put(t *testing.T, home, relative, data string) string {
	t.Helper()
	path := filepath.Join(home, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func skill(name string) string {
	return "---\nname: " + name + "\ndescription: Review a temporary fixture.\nmetadata:\n  harness: cursor, codex\n---\nKeep these original bytes.\n"
}

func TestDiscoverImportsCursorAndPortableSkill(t *testing.T) {
	home := t.TempDir()
	configPath := put(t, home, ".cursor/mcp.json", `{"unrelated":{"keep":true},"mcpServers":{"echo":{"command":"node","args":["path with spaces","quote\"","$5"],"env":{"MODE":"fixture"}}}}`)
	skillPath := put(t, home, ".cursor/skills/review/SKILL.md", skill("review"))
	assetPath := put(t, home, ".cursor/skills/review/assets/example.txt", "asset bytes\n")
	before := map[string][]byte{}
	for _, path := range []string{configPath, skillPath, assetPath} {
		before[path], _ = os.ReadFile(path)
	}
	plan, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Servers) != 1 || plan.Servers[0].Name != "echo" || !plan.Servers[0].Selected {
		t.Fatalf("servers: %+v; warnings: %+v", plan.Servers, plan.Warnings)
	}
	if len(plan.Skills) != 1 || plan.Skills[0].Name != "review" || !plan.Skills[0].Selected || plan.Skills[0].Fingerprint == "" {
		t.Fatalf("skills: %+v; warnings: %+v", plan.Skills, plan.Warnings)
	}
	if got := plan.Servers[0].Server.Args; !reflect.DeepEqual(got, []string{"path with spaces", "quote\"", "$5"}) {
		t.Fatalf("argv changed: %#v", got)
	}
	if err = plan.Revalidate(); err != nil {
		t.Fatal(err)
	}
	if err = VerifySkill(home, plan.Skills[0]); err != nil {
		t.Fatal(err)
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(want) {
			t.Fatalf("native source mutated: %s", path)
		}
	}
	if _, err = os.Stat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
		t.Fatalf("discovery wrote canonical directory: %v", err)
	}
	again, err := Discover(home)
	if err != nil || !reflect.DeepEqual(plan, again) {
		t.Fatalf("discovery is not deterministic: %v", err)
	}

	// The test, not Discover, writes canonical files to validate import semantics.
	raw, err := json.Marshal(map[string]any{"mcpServers": map[string]config.Server{"echo": plan.Servers[0].Server}})
	if err != nil {
		t.Fatal(err)
	}
	put(t, home, ".agentcfg/mcp.json", string(raw))
	put(t, home, ".agentcfg/skills/review/SKILL.md", string(before[skillPath]))
	loaded, err := config.Load(home)
	if err != nil || len(loaded.Servers) != 1 || len(loaded.Skills) != 1 || len(loaded.Warnings) != 0 {
		t.Fatalf("canonical import: %+v, %v", loaded, err)
	}
}

func TestDiscoverNativeFormats(t *testing.T) {
	tests := []struct {
		name, path, document, kind string
		args                       []string
	}{
		{"claude-code", ".claude.json", `{"mcpServers":{"fixture":{"type":"stdio","command":"node","args":["a b"]}},"projects":{"/project":{"mcpServers":{"not-user":{"command":"never"}}}}}`, "stdio", []string{"a b"}},
		{"codex", ".codex/config.toml", "[mcp_servers.fixture]\ncommand = 'node'\nargs = ['a b']\n[mcp_servers.fixture.env]\nMODE = 'fixture'\n", "stdio", []string{"a b"}},
		{"opencode", ".config/opencode/opencode.jsonc", "// preserved JSONC\n" + `{"mcp":{"fixture":{"type":"local","command":["node","a b"],"environment":{"MODE":"fixture"},"enabled":true,},},}`, "stdio", []string{"a b"}},
		{"gemini", ".gemini/settings.json", `{"mcpServers":{"fixture":{"httpUrl":"https://example.test/mcp","headers":{"X-Region":"test"}}}}`, "streamable-http", nil},
		{"antigravity", ".gemini/config/mcp_config.json", `{"mcpServers":{"fixture":{"serverUrl":"https://example.test/mcp"}}}`, "streamable-http", nil},
		{"cursor", ".cursor/mcp.json", `{"mcpServers":{"fixture":{"url":"https://example.test/mcp"}}}`, "streamable-http", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			put(t, home, tt.path, tt.document)
			plan, err := Discover(home)
			if err != nil || len(plan.Servers) != 1 {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
			got := plan.Servers[0]
			if got.Server.Type != tt.kind || !reflect.DeepEqual(got.Server.Args, tt.args) || !reflect.DeepEqual(got.Harnesses, []string{tt.name}) {
				t.Fatalf("candidate: %+v", got)
			}
		})
	}
}

func TestRevalidateDetectsSourceChanges(t *testing.T) {
	t.Run("mcp", func(t *testing.T) {
		home := t.TempDir()
		put(t, home, ".cursor/mcp.json", `{"mcpServers":{"fixture":{"command":"node"}}}`)
		plan, err := Discover(home)
		if err != nil {
			t.Fatal(err)
		}
		put(t, home, ".cursor/mcp.json", `{"mcpServers":{"fixture":{"command":"changed"}}}`)
		if err = plan.Revalidate(); err == nil {
			t.Fatal("changed MCP source accepted")
		}
		plan.Servers[0].Selected = false
		if err = plan.Revalidate(); err != nil {
			t.Fatal("unselected source must not block", err)
		}
	})
	t.Run("skill-asset", func(t *testing.T) {
		home := t.TempDir()
		put(t, home, ".agents/skills/review/SKILL.md", skill("review"))
		put(t, home, ".agents/skills/review/asset.txt", "before")
		plan, err := Discover(home)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Skills) != 1 {
			t.Fatalf("%+v", plan)
		}
		put(t, home, ".agents/skills/review/asset.txt", "after")
		if err = plan.Revalidate(); err == nil {
			t.Fatal("changed skill asset accepted")
		}
		if err = VerifySkill(home, plan.Skills[0]); err == nil {
			t.Fatal("changed fingerprint accepted")
		}
	})
}

func TestDiscoverCopiesCredentialValuesWithoutLeakingDiagnostics(t *testing.T) {
	const secret = "SENTINEL_DO_NOT_RETURN_THIS"
	home := t.TempDir()
	entries := map[string]any{
		"safe":         map[string]any{"command": "node"},
		"env":          map[string]any{"command": "node", "env": map[string]string{"API_KEY": secret}},
		"header":       map[string]any{"url": "https://example.test/mcp", "headers": map[string]string{"Authorization": secret}},
		"query":        map[string]any{"url": "https://example.test/mcp?key=" + secret},
		"userinfo":     map[string]any{"url": "https://user:" + secret + "@example.test/mcp"},
		"arg":          map[string]any{"command": "node", "args": []string{"--api-key", secret}},
		"custom-auth":  map[string]any{"url": "https://example.test/mcp", "headers": map[string]string{"X-Auth": secret}},
		"json-arg":     map[string]any{"command": "node", "args": []string{`{"apiKey":"` + secret + `"}`}},
		"database-arg": map[string]any{"command": "node", "args": []string{"postgres://user:" + secret + "@example.test/db"}},
		"symbolic":     map[string]any{"command": "node", "env": map[string]string{"HOME_COPY": "${HOME}"}},
		"disabled":     map[string]any{"command": "node", "disabled": true},
		"filtered":     map[string]any{"command": "node", "includeTools": []string{"safe"}},
		"relative":     map[string]any{"command": "./server"},
		"shell":        map[string]any{"command": "node server.js"},
		"proxy":        map[string]any{"command": "agentcfg", "args": []string{"connect", "--server", "x"}},
	}
	raw, _ := json.Marshal(map[string]any{"mcpServers": entries})
	put(t, home, ".cursor/mcp.json", string(raw))
	plan, err := Discover(home)
	if err != nil || len(plan.Servers) != 11 {
		t.Fatalf("expected all 11 representable entries, including the disabled definition, to be imported: %v", err)
	}
	visible, _ := json.Marshal(plan.Warnings)
	if strings.Contains(string(visible), secret) {
		t.Fatal("credential leaked into diagnostics")
	}
	imported := map[string]ServerCandidate{}
	for _, candidate := range plan.Servers {
		imported[candidate.Name] = candidate
	}
	if imported["env"].Server.Env["API_KEY"] != secret || imported["header"].Server.Headers["Authorization"] != secret || imported["arg"].Server.Args[1] != secret || imported["symbolic"].Server.Env["HOME_COPY"] != "${HOME}" {
		t.Fatal("native strings were changed during import")
	}
	for _, warning := range plan.Warnings {
		if imported[warning.Server].Name != "" && warning.BlocksInit([]string{"cursor"}) {
			t.Fatal("advisory still blocks valid import")
		}
	}
}

func TestConcurrentReadOnlyDiscover(t *testing.T) {
	home := t.TempDir()
	put(t, home, ".cursor/mcp.json", `{"mcpServers":{"fixture":{"command":"node"}}}`)
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			plan, err := Discover(home)
			if err != nil {
				t.Error(err)
				return
			}
			if err = plan.Revalidate(); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
}
