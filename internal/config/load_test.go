package config

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type testObject = map[string]any

func putFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func putJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, path, string(data))
}

func canonicalServers(t *testing.T, root string, servers testObject) {
	t.Helper()
	putJSON(t, filepath.Join(root, ".agents", "mcp.json"), testObject{"mcpServers": servers})
}

func pluginFixture(t *testing.T, root, folder, name string, servers testObject) string {
	t.Helper()
	path := filepath.Join(root, ".agents", "plugins", folder)
	putJSON(t, filepath.Join(path, "plugin.json"), testObject{"$schema": pluginSchema, "name": name})
	if servers != nil {
		putJSON(t, filepath.Join(path, "mcp.json"), testObject{"$schema": mcpSchema, "mcpServers": servers})
	}
	return path
}

func skillFixture(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	putFile(t, filepath.Join(path, "SKILL.md"), "---\nname: "+name+"\ndescription: Review the supplied source when asked.\n---\nRead the supplied source and report findings.\n")
	return path
}

func loadOK(t *testing.T, root string) *Config {
	t.Helper()
	result, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func expectWarnings(t *testing.T, config *Config, parts ...string) {
	t.Helper()
	all := strings.Join(config.Warnings, "\n")
	for _, part := range parts {
		if !strings.Contains(all, part) {
			t.Errorf("warnings lack %q:\n%s", part, all)
		}
	}
}

func serverNames(config *Config) []string { return sortedKeys(config.Servers) }

func skillNames(config *Config) []string {
	result := make([]string, 0, len(config.Skills))
	for _, skill := range config.Skills {
		result = append(result, skill.Name)
	}
	return result
}

func TestLoadDefaultsAndNoWrites(t *testing.T) {
	root := t.TempDir()
	config := loadOK(t, root)
	if !reflect.DeepEqual(config.Settings, defaultSettings()) {
		t.Fatalf("unexpected defaults: %#v", config.Settings)
	}
	if len(config.Servers) != 0 || len(config.Skills) != 0 || len(config.Warnings) != 0 {
		t.Fatalf("unexpected empty-root contents: %#v", config)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Load wrote files: %v, %v", entries, err)
	}

	pluginFixture(t, root, "demo", "demo", testObject{
		"never": testObject{"type": "stdio", "command": "agentmux-test-never-execute", "cwd": "${PLUGIN_DATA}/work"},
	})
	config = loadOK(t, root)
	server, exists := config.Servers["demo.never"]
	if !exists {
		t.Fatalf("bare executable should be resolved at launch, warnings: %v", config.Warnings)
	}
	if !strings.HasSuffix(server.Cwd, filepath.Join("demo", "work")) {
		t.Fatalf("cwd = %s", server.Cwd)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents", "data")); !os.IsNotExist(err) {
		t.Fatalf("Load created PLUGIN_DATA: %v", err)
	}
}

func TestCanonicalServersSettingsAndOverrides(t *testing.T) {
	root := t.TempDir()
	canonicalServers(t, root, testObject{
		"local":  testObject{"command": "node", "args": []string{"script.js"}, "harnesses": []string{"cursor"}, "env": testObject{"EXAMPLE": "value"}},
		"remote": testObject{"url": "https://example.test/mcp", "headers": testObject{"X-Literal": "${PLUGIN_ROOT}"}},
	})
	skillFixture(t, filepath.Join(root, ".agents", "skills"), "review")
		putFile(t, filepath.Join(root, ".agents", "skills", "review", "SKILL.md"), "---\nname: review\ndescription: Review code.\nmetadata:\n  harness: claude-code\n---\n")
	putJSON(t, filepath.Join(root, ".agents", "agentmux.json"), testObject{
		"harnesses": []string{"codex", "claude-code", "codex"},
		"runtime":   testObject{"idleTimeout": "30s", "requestTimeout": "2m"},
		"serverOverrides": testObject{
			"local":   testObject{"harnesses": []string{"codex"}, "sharing": "isolated"},
			"remote":  testObject{"harnesses": []string{}},
			"missing": testObject{"sharing": "shared"},
		},
	})
	config := loadOK(t, root)
	local := config.Servers["local"]
	if local.Type != "stdio" || local.Sharing != "isolated" || !reflect.DeepEqual(local.Harnesses, []string{"codex"}) {
		t.Fatalf("bad server override: %#v", local)
	}
	if local.Cwd != config.Root {
		t.Errorf("canonical cwd = %s, root = %s", local.Cwd, config.Root)
	}
	remote := config.Servers["remote"]
	if remote.Type != "streamable-http" || remote.Sharing != "shared" || remote.Headers["X-Literal"] != "${PLUGIN_ROOT}" {
		t.Fatalf("bad remote: %#v", remote)
	}
	if !reflect.DeepEqual(config.Settings.Harnesses, []string{"codex", "claude-code"}) {
		t.Fatal(config.Settings.Harnesses)
	}
	if config.Settings.Runtime.IdleTimeout != "30s" || config.Settings.Runtime.RequestTimeout != "2m" {
		t.Fatal(config.Settings.Runtime)
	}
	if len(config.Skills) != 1 || !reflect.DeepEqual(config.Skills[0].Harnesses, []string{"claude-code"}) {
		t.Fatal(config.Skills)
	}
	if !Allows(local.Harnesses, "") || Allows(local.Harnesses, "cursor") || !Allows(remote.Harnesses, "cursor") {
		t.Fatal("incorrect filter behavior")
	}
	expectWarnings(t, config, "missing", "does not match")
}

func TestInvalidEntriesAreIsolated(t *testing.T) {
	root := t.TempDir()
	canonicalServers(t, root, testObject{
		"valid":       testObject{"type": "stdio", "command": "node"},
		"bad-command": testObject{"type": "stdio", "command": ""},
		"bad-env":     testObject{"type": "stdio", "command": "node", "env": testObject{"BAD": 1}},
		"bad-field":   testObject{"type": "stdio", "command": "node", "url": "https://example.test"},
		"bad-type":    testObject{"type": "sockets", "command": "node"},
		"bad-args":    testObject{"type": "stdio", "command": "node", "args": []any{1}},
		"legacy":      testObject{"type": "sse", "url": "https://example.test/sse"},
		"null":        nil,
	})
	config := loadOK(t, root)
	if !reflect.DeepEqual(serverNames(config), []string{"valid"}) {
		t.Fatalf("servers = %v", serverNames(config))
	}
	if len(config.Warnings) != 7 {
		t.Fatalf("warnings = %v", config.Warnings)
	}
	expectWarnings(t, config, "legacy", "unsupported", "bad-command", "bad-field")

	putFile(t, filepath.Join(root, ".agents", "mcp.json"),
		"{\"mcpServers\":{\"valid\":{\"command\":\"node\"},\"duplicate\":{\"command\":\"node\",\"command\":\"other\"}}}")
	config = loadOK(t, root)
	if !reflect.DeepEqual(serverNames(config), []string{"valid"}) {
		t.Fatal(serverNames(config))
	}
	expectWarnings(t, config, "duplicate JSON key")
}

func TestPortablePluginVariablesAndNamespaces(t *testing.T) {
	root := filepath.Join(t.TempDir(), "literal-${PLUGIN_DATA}")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	plugin := pluginFixture(t, root, "installed-folder", "acme.tools", testObject{
		"inspect": testObject{"type": "stdio", "command": "./bin/runner", "cwd": "${PLUGIN_ROOT}",
			"args": []string{"${PLUGIN_ROOT}", "${PLUGIN_DATA}", "${UNKNOWN}", "../../opaque"},
			"env":  testObject{"CONFIG": "${PLUGIN_ROOT}/settings.json", "OPAQUE": "../../outside"}},
	})
	putFile(t, filepath.Join(plugin, "bin", "runner"), "This fixture must never be executed.\n")
	skill := skillFixture(t, filepath.Join(plugin, "skills"), "review")
		putFile(t, filepath.Join(skill, "SKILL.md"), "---\nname: review\ndescription: Review code.\nmetadata:\n  harness: codex\n---\n")
	putJSON(t, filepath.Join(root, ".agents", "agentmux.json"), testObject{
		"serverOverrides": testObject{"acme.tools.inspect": testObject{"harnesses": []string{"cursor"}, "sharing": "isolated"}},
	})
	config := loadOK(t, root)
	if len(config.Warnings) != 0 {
		t.Fatal(config.Warnings)
	}
	server := config.Servers["acme.tools.inspect"]
	resolvedPlugin, _ := filepath.EvalSymlinks(plugin)
	resolvedSkill, _ := filepath.EvalSymlinks(skill)
	if server.Command != filepath.Join(resolvedPlugin, "bin", "runner") || server.Cwd != resolvedPlugin {
		t.Fatalf("bad paths: %#v", server)
	}
	data := filepath.Join(config.Root, ".agents", "data", "plugins", "acme.tools")
	if !reflect.DeepEqual(server.Args, []string{resolvedPlugin, data, "${UNKNOWN}", "../../opaque"}) {
		t.Fatal(server.Args)
	}
	if server.Env["PLUGIN_ROOT"] != resolvedPlugin || server.Env["PLUGIN_DATA"] != data || server.Env["CONFIG"] != resolvedPlugin+"/settings.json" {
		t.Fatal(server.Env)
	}
	if server.Env["OPAQUE"] != "../../outside" || server.Sharing != "isolated" || !reflect.DeepEqual(server.Harnesses, []string{"cursor"}) {
		t.Fatalf("bad expansion/override: %#v", server)
	}
	if !reflect.DeepEqual(skillNames(config), []string{"acme.tools.review"}) || config.Skills[0].Path != resolvedSkill {
		t.Fatal(config.Skills)
	}
	if !reflect.DeepEqual(config.Skills[0].Harnesses, []string{"codex"}) {
		t.Fatal(config.Skills[0].Harnesses)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("plugin data created during loading")
	}
}

func TestPortablePluginStrictFieldsAndComponentIsolation(t *testing.T) {
	root := t.TempDir()
	plugin := pluginFixture(t, root, "portable", "portable", testObject{
		"valid":         testObject{"type": "stdio", "command": "node"},
		"filter":        testObject{"type": "stdio", "command": "node", "harnesses": []string{"codex"}},
		"sharing":       testObject{"type": "stdio", "command": "node", "sharing": "shared"},
		"implicit":      testObject{"command": "node"},
		"reserved":      testObject{"type": "stdio", "command": "node", "env": testObject{"PLUGIN_ROOT": "/other"}},
		"case-reserved": testObject{"type": "stdio", "command": "node", "env": testObject{"plugin_data": "/other"}},
		"remote-cwd":    testObject{"type": "streamable-http", "url": "https://example.test", "cwd": "./"},
	})
	skillFixture(t, filepath.Join(plugin, "skills"), "review")
	config := loadOK(t, root)
	if !reflect.DeepEqual(serverNames(config), []string{"portable.valid"}) || len(config.Skills) != 1 {
		t.Fatalf("%v, %v", serverNames(config), config.Skills)
	}
	expectWarnings(t, config, "serverOverrides", "required", "reserved")
	putJSON(t, filepath.Join(plugin, "mcp.json"), testObject{"$schema": mcpSchema, "mcpServers": testObject{}, "harnesses": []string{"codex"}})
	config = loadOK(t, root)
	if len(config.Servers) != 0 || len(config.Skills) != 1 {
		t.Fatal("bad MCP document disabled unrelated skills")
	}
	expectWarnings(t, config, "unknown field")
	putJSON(t, filepath.Join(plugin, "mcp.json"), testObject{"mcpServers": testObject{}})
	expectWarnings(t, loadOK(t, root), "$schema")
}

func TestManifestValidationAndWarnings(t *testing.T) {
	root := t.TempDir()
	plugin := pluginFixture(t, root, "good", "good", testObject{"ok": testObject{"type": "stdio", "command": "node"}})
	putJSON(t, filepath.Join(plugin, "plugin.json"), testObject{
		"$schema": pluginSchema, "name": "good", "version": "not-semver", "homepage": "not-a-url",
		"author": testObject{"email": "not-an-email"}, "extensions": false,
		"mcpServers": testObject{"injected": testObject{"command": "do-not-run"}}, "harnesses": []string{"codex"},
	})
	bad := pluginFixture(t, root, "bad", "bad", testObject{"never": testObject{"type": "stdio", "command": "node"}})
	skillFixture(t, filepath.Join(bad, "skills"), "review")
	putJSON(t, filepath.Join(bad, "plugin.json"), testObject{"$schema": pluginSchema, "name": "bad", "author": testObject{"extra": true}})
	config := loadOK(t, root)
	if !reflect.DeepEqual(serverNames(config), []string{"good.ok"}) || len(config.Skills) != 0 {
		t.Fatal(serverNames(config), config.Skills)
	}
	expectWarnings(t, config, "unknown manifest field", "non-object extensions", "author")
	putJSON(t, filepath.Join(plugin, "plugin.json"), testObject{"$schema": pluginSchema, "name": "good", "extensions": testObject{"unimplemented": 42}})
	config = loadOK(t, root)
	if _, exists := config.Servers["good.ok"]; !exists {
		t.Fatal("unimplemented extension namespace was validated")
	}
}

func TestManifestInvalidValuesRejectWholePlugin(t *testing.T) {
	cases := []testObject{
		{"name": "demo"},
		{"$schema": pluginSchema, "name": "Bad"},
		{"$schema": pluginSchema, "name": "bad..name"},
		{"$schema": pluginSchema, "name": "bad--name"},
		{"$schema": pluginSchema, "name": "demo", "version": 1},
		{"$schema": pluginSchema, "name": "demo", "author": nil},
		{"$schema": pluginSchema, "name": "demo", "keywords": []any{1}},
		{"$schema": "https://example.test/unrecognized-schema", "name": "demo"},
	}
	for i, manifest := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			root := t.TempDir()
			plugin := pluginFixture(t, root, "demo", "demo", testObject{"server": testObject{"type": "stdio", "command": "node"}})
			skillFixture(t, filepath.Join(plugin, "skills"), "review")
			putJSON(t, filepath.Join(plugin, "plugin.json"), manifest)
			config := loadOK(t, root)
			if len(config.Servers) != 0 || len(config.Skills) != 0 || len(config.Warnings) != 1 {
				t.Fatalf("invalid manifest accepted: %#v", config)
			}
		})
	}
}

func TestDeterministicConflicts(t *testing.T) {
	root := t.TempDir()
	canonicalServers(t, root, testObject{"demo.server": testObject{"command": "canonical"}})
	for _, folder := range []string{"z-last", "a-first"} {
		plugin := pluginFixture(t, root, folder, "demo", testObject{
			"server": testObject{"type": "stdio", "command": folder},
			"other":  testObject{"type": "stdio", "command": folder},
		})
		skillFixture(t, filepath.Join(plugin, "skills"), "review")
	}
	first := loadOK(t, root)
	second := loadOK(t, root)
	if first.Servers["demo.server"].Command != "canonical" || first.Servers["demo.other"].Command != "a-first" {
		t.Fatal(first.Servers)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("loading is not deterministic")
	}
	if len(first.Skills) != 1 {
		t.Fatal(first.Skills)
	}
	expectWarnings(t, first, "collides", "duplicate plugin name")
}

func TestPluginTraversalAndCommandValidation(t *testing.T) {
	root := t.TempDir()
	plugin := pluginFixture(t, root, "demo", "demo", testObject{
		"ok":                  testObject{"type": "stdio", "command": "node"},
		"command-parent":      testObject{"type": "stdio", "command": "./../outside"},
		"command-absolute":    testObject{"type": "stdio", "command": filepath.Join(root, "outside")},
		"command-placeholder": testObject{"type": "stdio", "command": "${PLUGIN_ROOT}/runner"},
		"command-shell":       testObject{"type": "stdio", "command": "node --version"},
		"cwd-parent":          testObject{"type": "stdio", "command": "node", "cwd": "./../"},
		"cwd-root-parent":     testObject{"type": "stdio", "command": "node", "cwd": "${PLUGIN_ROOT}/.."},
		"cwd-data-parent":     testObject{"type": "stdio", "command": "node", "cwd": "${PLUGIN_DATA}/.."},
		"cwd-bare":            testObject{"type": "stdio", "command": "node", "cwd": "relative"},
		"cwd-empty":           testObject{"type": "stdio", "command": "node", "cwd": ""},
	})
	putFile(t, filepath.Join(filepath.Dir(plugin), "outside"), "fixture")
	putFile(t, filepath.Join(root, "outside"), "fixture")
	config := loadOK(t, root)
	if !reflect.DeepEqual(serverNames(config), []string{"demo.ok"}) {
		t.Fatalf("unsafe entries loaded: %v", serverNames(config))
	}
	if len(config.Warnings) != 9 {
		t.Fatal(config.Warnings)
	}
	expectWarnings(t, config, "escapes permitted root", "command-placeholder", "cwd-empty")
}

func TestSettingsInvalidValues(t *testing.T) {
	cases := []string{
		"null", "[]", "{} {}",
		"{\"unknown\":true}", "{\"harnesses\":null}", "{\"harnesses\":[\"unknown\"]}",
		"{\"runtime\":{\"idleTimeout\":\"0s\"}}", "{\"runtime\":{\"requestTimeout\":\"invalid\"}}",
		"{\"runtime\":{\"other\":\"1m\"}}", "{\"runtime\":null}",
		"{\"skillOverrides\":{\"review\":{\"sharing\":\"isolated\"}}}",
		"{\"serverOverrides\":{\"server\":{\"harnesses\":[\"unknown\"]}}}",
		"{\"harnesses\":[],\"harnesses\":[\"codex\"]}",
	}
	for i, input := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			root := t.TempDir()
			putFile(t, filepath.Join(root, ".agents", "agentmux.json"), input)
			if _, err := Load(root); err == nil {
				t.Fatalf("accepted invalid settings: %s", input)
			}
		})
	}
	root := t.TempDir()
	putJSON(t, filepath.Join(root, ".agents", "agentmux.json"), testObject{"harnesses": []string{}})
	if config := loadOK(t, root); config.Settings.Harnesses == nil || len(config.Settings.Harnesses) != 0 {
		t.Fatal("empty global harness list did not remain explicit")
	}
}

func TestRemoteURLAndHeaderValidation(t *testing.T) {
	valid := []string{"https://example.test/mcp", "http://localhost:8080/mcp", "http://127.23.4.5/mcp", "http://[::1]:8080/mcp", "https://example.test/${PLUGIN_DATA}"}
	invalid := []string{"", "/mcp", "ftp://example.test/mcp", "http://example.test/mcp", "http://localhost.evil/mcp",
		"http://127.0.0.1.evil/mcp", "http://0.0.0.0/mcp", "https://user:password@example.test/mcp",
		"https://example.test/mcp#fragment", "https://example.test/mcp#", "https://example.test:99999/mcp",
		"https://example.test:/mcp", "https://[not-an-ip]/mcp", "https://example.test\\evil/mcp"}
	for _, value := range valid {
		if err := validateURL(value); err != nil {
			t.Errorf("valid URL %q: %v", value, err)
		}
	}
	for _, value := range invalid {
		if err := validateURL(value); err == nil {
			t.Errorf("invalid URL accepted: %q", value)
		}
	}
	for _, headers := range []map[string]string{
		{"X-Key": "one", "x-key": "two"}, {"Bad Name": "value"}, {"X-Key": "one\r\nInjected: value"}, {"": "empty"}, {"X-Key": "\x00"},
	} {
		if err := validateHeaders(headers); err == nil {
			t.Errorf("invalid headers accepted: %v", headers)
		}
	}
	if err := validateHeaders(map[string]string{"X-Literal": "${PLUGIN_ROOT}", "X-Tab": "one\ttwo"}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	canonicalServers(t, root, testObject{"good": testObject{"url": valid[0]}, "bad": testObject{"url": invalid[3]}})
	if config := loadOK(t, root); !reflect.DeepEqual(serverNames(config), []string{"good"}) {
		t.Fatal(config.Servers)
	}
}

func TestSkillFrontmatterAndIsolation(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, ".agents", "skills")
	skillFixture(t, parent, "valid")
	skillFixture(t, filepath.Join(parent, "nested"), "not-discovered")
	bad := map[string]string{
		"missing":       "# No frontmatter",
		"wrong-name":    "---\nname: other\ndescription: Review code.\n---\n",
		"bad-type":      "---\nname: bad-type\ndescription: true\n---\n",
		"duplicate":     "---\nname: duplicate\nname: duplicate\ndescription: Review code.\n---\n",
		"alias":         "---\nname: alias\ndescription: &description Review code.\nlicense: *description\n---\n",
		"metadata":      "---\nname: metadata\ndescription: Review code.\nmetadata:\n  version: 1\n---\n",
		"compatibility": "---\nname: compatibility\ndescription: Review code.\ncompatibility: ''\n---\n",
		"no-end":        "---\nname: no-end\ndescription: Review code.\n",
		"long":          "---\nname: long\ndescription: " + strings.Repeat("x", 1025) + "\n---\n",
	}
	for name, content := range bad {
		putFile(t, filepath.Join(parent, name, "SKILL.md"), content)
	}
	config := loadOK(t, root)
	if !reflect.DeepEqual(skillNames(config), []string{"valid"}) {
		t.Fatal(config.Skills)
	}
	if len(config.Warnings) != len(bad) {
		t.Fatal(config.Warnings)
	}
	crlf := "\xef\xbb\xbf---\r\nname: unicode\r\ndescription: \"éèà\"\r\nmetadata:\r\n  version: \"1\"\r\n---\r\n"
	if name, err := validateSkill([]byte(crlf), "unicode"); err != nil || name != "unicode" {
		t.Fatalf("%s: %v", name, err)
	}
	if _, err := validateSkill([]byte("---\nname: évaluer\ndescription: Review code.\n---\n"), "évaluer"); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedSkillCopiesAreNotRediscovered(t *testing.T) {
	root := t.TempDir()
	plugin := pluginFixture(t, root, "demo", "demo", nil)
	skillFixture(t, filepath.Join(plugin, "skills"), "review")
	generated := skillFixture(t, filepath.Join(root, ".agents", "skills"), "review")
	putJSON(t, filepath.Join(generated, originMarker), testObject{"managedBy": "agentmux"})
	config := loadOK(t, root)
	if !reflect.DeepEqual(skillNames(config), []string{"demo.review"}) {
		t.Fatal(config.Skills)
	}
	if len(config.Warnings) != 0 {
		t.Fatal(config.Warnings)
	}
}

func directoryLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err == nil {
		return
	} else if runtime.GOOS != "windows" {
		t.Skipf("symlink unavailable: %v", err)
	}
	// Windows junction creation needs no symlink privilege. Both paths are
	// test-owned temporary directories; this never launches an MCP server.
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("junction unavailable: %v: %s", err, output)
	}
}

func TestPluginDirectoryLinkEscapes(t *testing.T) {
	for _, component := range []string{"command", "cwd", "skills", "skill-assets", "data"} {
		t.Run(component, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			putFile(t, filepath.Join(outside, "runner"), "fixture")
			plugin := pluginFixture(t, root, "demo", "demo", testObject{"ok": testObject{"type": "stdio", "command": "node"}})
			servers := testObject{"ok": testObject{"type": "stdio", "command": "node"}}
			switch component {
			case "command":
				directoryLink(t, outside, filepath.Join(plugin, "bin"))
				servers["escape"] = testObject{"type": "stdio", "command": "./bin/runner"}
			case "cwd":
				directoryLink(t, outside, filepath.Join(plugin, "workspace"))
				servers["escape"] = testObject{"type": "stdio", "command": "node", "cwd": "./workspace"}
			case "skills":
				skillFixture(t, outside, "review")
				directoryLink(t, outside, filepath.Join(plugin, "skills"))
			case "skill-assets":
				skill := skillFixture(t, filepath.Join(plugin, "skills"), "review")
				directoryLink(t, outside, filepath.Join(skill, "assets"))
			case "data":
				directoryLink(t, outside, filepath.Join(root, ".agents", "data", "plugins", "demo"))
			}
			putJSON(t, filepath.Join(plugin, "mcp.json"), testObject{"$schema": mcpSchema, "mcpServers": servers})
			config := loadOK(t, root)
			expected := []string{"demo.ok"}
			if component == "data" {
				expected = []string{}
			}
			if !reflect.DeepEqual(serverNames(config), expected) || len(config.Skills) != 0 {
				t.Fatal(config.Servers, config.Skills)
			}
			expectWarnings(t, config, "escapes permitted root")
		})
	}
}

func TestContainedSkillLinkAndGeneratedLink(t *testing.T) {
	root := t.TempDir()
	plugin := pluginFixture(t, root, "demo", "demo", nil)
	actual := skillFixture(t, filepath.Join(plugin, "assets"), "review")
	directoryLink(t, actual, filepath.Join(plugin, "skills", "review"))
	directoryLink(t, actual, filepath.Join(root, ".agents", "skills", "review"))
	config := loadOK(t, root)
	if !reflect.DeepEqual(skillNames(config), []string{"demo.review"}) || len(config.Warnings) != 0 {
		t.Fatal(config.Skills, config.Warnings)
	}
}

func TestManifestAndSkillFileLinkEscapes(t *testing.T) {
	for _, targetFile := range []string{"plugin.json", "skills/review/SKILL.md", "mcp.json"} {
		t.Run(targetFile, func(t *testing.T) {
			root := t.TempDir()
			plugin := pluginFixture(t, root, "demo", "demo", testObject{"ok": testObject{"type": "stdio", "command": "node"}})
			skillFixture(t, filepath.Join(plugin, "skills"), "review")
			outside := filepath.Join(t.TempDir(), "file")
			putFile(t, outside, "outside")
			link := filepath.Join(plugin, filepath.FromSlash(targetFile))
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Skipf("file symlink unavailable: %v", err)
			}
			config := loadOK(t, root)
			expectWarnings(t, config, "escapes permitted root")
			switch targetFile {
			case "plugin.json":
				if len(config.Servers) != 0 || len(config.Skills) != 0 {
					t.Fatal("escaping manifest did not reject plugin")
				}
			case "mcp.json":
				if len(config.Servers) != 0 || len(config.Skills) != 1 {
					t.Fatal("escaping MCP component did not isolate failure")
				}
			default:
				if len(config.Servers) != 1 || len(config.Skills) != 0 {
					t.Fatal("escaping skill did not isolate failure")
				}
			}
		})
	}
}
