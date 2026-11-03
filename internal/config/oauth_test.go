package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validOAuthJSON = `{"account":"work","clientId":"agentcfg-client","issuer":"https://auth.example.test","redirectUrl":"http://127.0.0.1:48761/callback","scopes":["tools.read"]}`

func writeOAuthFixture(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadOAuthIsolatesInvalidCanonicalEntries(t *testing.T) {
	root := t.TempDir()
	writeOAuthFixture(t, root, ".agentcfg/mcp.json", `{"mcpServers":{
		"valid":{"url":"https://mcp.example.test/mcp","oauth":`+validOAuthJSON+`},
		"plain":{"command":"go"},
		"stdio-oauth":{"command":"go","oauth":`+validOAuthJSON+`},
		"literal-header":{"url":"https://mcp.example.test/mcp","headers":{"aUtHoRiZaTiOn":"secret"},"oauth":`+validOAuthJSON+`},
		"bad-oauth":{"url":"https://mcp.example.test/mcp","oauth":{"clientId":"client","redirectUrl":"http://public.example.test/callback"}}
	}}`)
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 2 || cfg.Servers["plain"].Command != "go" {
		t.Fatalf("invalid entries not isolated: %v", cfg.Servers)
	}
	server, ok := cfg.Servers["valid"]
	if !ok || server.OAuth == nil || server.OAuth.Account != "work" || server.OAuth.ClientID != "agentcfg-client" {
		t.Fatalf("missing OAuth config: %+v", server)
	}
	if len(cfg.Warnings) != 3 {
		t.Fatalf("warnings: %v", cfg.Warnings)
	}
}

func TestPortableOAuthRejectedButExternalOverrideAccepted(t *testing.T) {
	root := t.TempDir()
	writeOAuthFixture(t, root, ".agentcfg/plugins/demo/plugin.json", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo"}`)
	writeOAuthFixture(t, root, ".agentcfg/plugins/demo/mcp.json", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{
		"remote":{"type":"streamable-http","url":"https://mcp.example.test/mcp"},
		"nonportable":{"type":"streamable-http","url":"https://mcp.example.test/mcp","oauth":`+validOAuthJSON+`}
	}}`)
	writeOAuthFixture(t, root, ".agentcfg/agentcfg.json", `{"harnesses":["antigravity"],"serverOverrides":{"demo.remote":{"harnesses":["antigravity"],"oauth":`+validOAuthJSON+`}}}`)
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("portable entry isolation: %v", cfg.Servers)
	}
	server, ok := cfg.Servers["demo.remote"]
	if !ok || server.OAuth == nil || server.OAuth.Account != "work" ||
		!reflect.DeepEqual(server.Harnesses, []string{"antigravity"}) {
		t.Fatalf("override: %+v", server)
	}
	if !reflect.DeepEqual(cfg.Settings.Harnesses, []string{"antigravity"}) {
		t.Fatal(cfg.Settings.Harnesses)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "oauth") {
		t.Fatal(cfg.Warnings)
	}
}

func TestOAuthOverrideCannotSilentlyLeaveUnsafeServerEnabled(t *testing.T) {
	for _, entry := range []string{
		`{"command":"go"}`,
		`{"url":"https://mcp.example.test/mcp","headers":{"Proxy-Authorization":"secret"}}`,
	} {
		t.Run(entry, func(t *testing.T) {
			root := t.TempDir()
			writeOAuthFixture(t, root, ".agentcfg/mcp.json", `{"mcpServers":{"unsafe":`+entry+`,"plain":{"command":"go"}}}`)
			writeOAuthFixture(t, root, ".agentcfg/agentcfg.json", `{"serverOverrides":{"unsafe":{"oauth":`+validOAuthJSON+`}}}`)
			cfg, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := cfg.Servers["unsafe"]; ok || len(cfg.Servers) != 1 || len(cfg.Warnings) != 1 {
				t.Fatalf("unsafe override: %v %v", cfg.Servers, cfg.Warnings)
			}
		})
	}
}

func TestOAuthValidation(t *testing.T) {
	invalid := []string{
		`null`, `true`,
		`{"clientId":"client","redirectUrl":"http://127.0.0.1:48761/callback","clientSecret":"inline-secret"}`,
		`{"clientId":"client","redirectUrl":"http://127.0.0.1:48761/callback","account":"bad\u001baccount"}`,
		`{"clientId":"client","redirectUrl":"http://127.0.0.1:48761/callback","scopes":["two scopes"]}`,
		`{"clientId":"client","redirectUrl":"http://127.0.0.1:48761/callback","scopes":"one"}`,
		`{"clientId":"client","redirectUrl":"http://127.0.0.1:48761/callback","clientSecretEnv":"CLIENT_SECRET"}`,
		`{"dynamicRegistration":true,"issuer":"https://auth.example.test","redirectUrl":"http://127.0.0.1:48761/callback"}`,
		`{"redirectUrl":"http://127.0.0.1:48761/callback"}`,
		`{"dynamicRegistration":"true","redirectUrl":"http://127.0.0.1:48761/callback"}`,
		`{"dynamicRegistration":null,"clientId":"client","redirectUrl":"http://127.0.0.1:48761/callback"}`,
		`{"clientMetadataUrl":"http://localhost/client.json","redirectUrl":"http://127.0.0.1:48761/callback"}`,
		`{"clientId":"client","redirectUrl":"https://user:pass@example.test/callback"}`,
		`{"clientId":"client","redirectUrl":"http://127.0.0.1:0/callback"}`,
		`{"clientId":"client","redirectUrl":"https://example.test/callback?state=attacker"}`,
		`{"clientId":"client","redirectUrl":"https://example.test/callback#fragment"}`,
		`{"clientId":"client","issuer":"https://example.test?issuer=other","redirectUrl":"https://example.test/callback"}`,
		`{"clientId":"client","redirectUrl":"file:///tmp/callback"}`,
	}
	for _, raw := range invalid {
		if _, err := parseOAuth(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted invalid OAuth: %s", raw)
		}
	}
	for _, raw := range []string{
		validOAuthJSON,
		`{"clientMetadataUrl":"https://client.example.test/client.json","redirectUrl":"https://client.example.test/callback"}`,
		`{"dynamicRegistration":true,"redirectUrl":"http://127.0.0.1:48761/callback"}`,
		`{"clientId":"client","issuer":"https://auth.example.test","clientSecretEnv":"MISSING_TEST_SECRET_ENV","redirectUrl":"http://127.0.0.1:48761/callback"}`,
	} {
		if _, err := parseOAuth(json.RawMessage(raw)); err != nil {
			t.Errorf("valid OAuth rejected: %s: %v", raw, err)
		}
	}
}

func TestSkillNoneAndAntigravityCompatibilityWithoutRewriting(t *testing.T) {
	root := t.TempDir()
	skill := "---\nname: review\ndescription: Review code.\nmetadata:\n  harness: none\n---\n# Keep this content exactly.\n"
	writeOAuthFixture(t, root, ".agentcfg/skills/review/SKILL.md", skill)
	cfg, err := Load(root)
	if err != nil || len(cfg.Skills) != 1 {
		t.Fatalf("load: %+v %v", cfg, err)
	}
	if !reflect.DeepEqual(cfg.Skills[0].Harnesses, []string{"none"}) || Allows(cfg.Skills[0].Harnesses, "antigravity") || Allows(cfg.Skills[0].Harnesses, "codex") {
		t.Fatal(cfg.Skills[0])
	}
	after, err := os.ReadFile(filepath.Join(root, ".agentcfg", "skills", "review", "SKILL.md"))
	if err != nil || string(after) != skill {
		t.Fatalf("skill changed: %v", err)
	}
	enabled := []byte(strings.Replace(skill, "harness: none", "harness: antigravity, codex", 1))
	harnesses, err := skillHarnesses(enabled)
	if err != nil || !reflect.DeepEqual(harnesses, []string{"antigravity", "codex"}) {
		t.Fatalf("Antigravity metadata: %v %v", harnesses, err)
	}
	for _, metadata := range []string{"metadata: null\n", "metadata: scalar\n", "metadata: [one, two]\n", "metadata:\n  harness: [none]\n", "metadata:\n  harness: none, codex\n"} {
		data := []byte("---\nname: review\ndescription: Review code.\n" + metadata + "---\n")
		if _, err := skillHarnesses(data); err == nil {
			t.Errorf("invalid metadata accepted: %s", metadata)
		}
	}
}
