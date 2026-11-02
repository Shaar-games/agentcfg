package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestInvalidServerValueCannotPoisonDocument(t *testing.T) {
	root := t.TempDir()
	nested := strings.Repeat("[", 100) + "0" + strings.Repeat("]", 100)
	canonicalServers(t, root, testObject{
		"good":         testObject{"command": "node"},
		"huge-number":  testObject{"command": json.RawMessage("1e400")},
		"nested-field": testObject{"command": "node", "unknown": json.RawMessage(nested)},
	})
	config := loadOK(t, root)
	if !reflect.DeepEqual(serverNames(config), []string{"good"}) {
		t.Fatal(config.Servers)
	}
	if len(config.Warnings) != 2 {
		t.Fatal(config.Warnings)
	}
}

func TestRejectedPluginCannotHideStandaloneSkills(t *testing.T) {
	root := t.TempDir()
	skillFixture(t, filepath.Join(root, ".agents", "skills"), "review")
	directoryLink(t, filepath.Join(root, ".agents"), filepath.Join(root, ".agents", "plugins", "bad"))
	config := loadOK(t, root)
	if !reflect.DeepEqual(skillNames(config), []string{"review"}) {
		t.Fatalf("rejected plugin hid canonical skills: %v", config.Skills)
	}
	expectWarnings(t, config, "plugin.json")
}

func TestEmptySharingOverrideIsRejected(t *testing.T) {
	root := t.TempDir()
	canonicalServers(t, root, testObject{"private": testObject{"command": "node", "sharing": "isolated"}})
	putJSON(t, filepath.Join(root, ".agents", "agentmux.json"), testObject{
		"serverOverrides": testObject{"private": testObject{"sharing": ""}},
	})
	if _, err := Load(root); err == nil {
		t.Fatal("empty override silently changed sharing")
	}
}

func TestVSCodeExplicitHarness(t *testing.T) {
	root := t.TempDir()
	canonicalServers(t, root, testObject{"local": testObject{"command": "node", "harnesses": []string{"vscode"}}})
	putJSON(t, filepath.Join(root, ".agents", "agentmux.json"), testObject{"harnesses": []string{"vscode"}})
	config := loadOK(t, root)
	if len(config.Warnings) != 0 || !reflect.DeepEqual(config.Settings.Harnesses, []string{"vscode"}) {
		t.Fatal(config.Warnings, config.Settings.Harnesses)
	}
	if !Allows(config.Servers["local"].Harnesses, "vscode") {
		t.Fatal("vscode filter not accepted")
	}
}

func TestPluginDataMustBeDedicatedDirectory(t *testing.T) {
	for _, kind := range []string{"file", "sibling-link"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			pluginFixture(t, root, "demo", "demo", testObject{"local": testObject{"type": "stdio", "command": "node"}})
			path := filepath.Join(root, ".agents", "data", "plugins", "demo")
			if kind == "file" {
				putFile(t, path, "not a directory")
			} else {
				sibling := filepath.Join(filepath.Dir(path), "other")
				if err := os.MkdirAll(sibling, 0o700); err != nil {
					t.Fatal(err)
				}
				directoryLink(t, sibling, path)
			}
			config := loadOK(t, root)
			if len(config.Servers) != 0 {
				t.Fatal(config.Servers)
			}
			expectWarnings(t, config, "PLUGIN_DATA")
		})
	}
}

func TestRelativeCwdExpandsBeforeResolution(t *testing.T) {
	root := t.TempDir()
	plugin := pluginFixture(t, root, "demo", "demo", testObject{
		"literal": testObject{"type": "stdio", "command": "node", "cwd": "./${PLUGIN_DATA}"},
		"ok":      testObject{"type": "stdio", "command": "node"},
	})
	// A literal placeholder-named directory must not suppress expansion.
	if err := os.MkdirAll(filepath.Join(plugin, "${PLUGIN_DATA}"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := loadOK(t, root)
	if !reflect.DeepEqual(serverNames(config), []string{"demo.ok"}) {
		t.Fatal(config.Servers)
	}
	expectWarnings(t, config, "literal", "cwd")
}

func TestSkillAliasPreservesInstallBasename(t *testing.T) {
	root := t.TempDir()
	plugin := pluginFixture(t, root, "demo", "demo", nil)
	actual := filepath.Join(plugin, "payload")
	putFile(t, filepath.Join(actual, "SKILL.md"), "---\nname: review\ndescription: Review source when asked.\n---\n")
	discovered := filepath.Join(plugin, "skills", "review")
	directoryLink(t, actual, discovered)
	config := loadOK(t, root)
	if !reflect.DeepEqual(skillNames(config), []string{"demo.review"}) || len(config.Warnings) != 0 {
		t.Fatal(config.Skills, config.Warnings)
	}
	if filepath.Base(config.Skills[0].Path) != "review" {
		t.Fatal("resolved target basename would violate skill frontmatter at install time")
	}
}

func TestIPv6AndMalformedAuthority(t *testing.T) {
	for _, value := range []string{"https://[2001:db8::1]/mcp", "https://[fe80::1%25eth0]/mcp"} {
		if err := validateURL(value); err != nil {
			t.Errorf("valid IP literal %q: %v", value, err)
		}
	}
	for _, value := range []string{"https://example.test:443:443/mcp", "https://2001:db8::1/mcp", "https://[127.0.0.1]/mcp"} {
		if err := validateURL(value); err == nil {
			t.Errorf("invalid authority accepted: %s", value)
		}
	}
}

func TestWrongComponentKindsStayIsolated(t *testing.T) {
	root := t.TempDir()
	mcpWrong := pluginFixture(t, root, "mcp-wrong", "mcp-wrong", nil)
	if err := os.Mkdir(filepath.Join(mcpWrong, "mcp.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	skillFixture(t, filepath.Join(mcpWrong, "skills"), "review")
	skillsWrong := pluginFixture(t, root, "skills-wrong", "skills-wrong", testObject{"local": testObject{"type": "stdio", "command": "node"}})
	putFile(t, filepath.Join(skillsWrong, "skills"), "not a directory")
	config := loadOK(t, root)
	if !reflect.DeepEqual(serverNames(config), []string{"skills-wrong.local"}) || !reflect.DeepEqual(skillNames(config), []string{"mcp-wrong.review"}) {
		t.Fatal(config.Servers, config.Skills)
	}
	if len(config.Warnings) != 2 {
		t.Fatal(config.Warnings)
	}
}

func TestSkillTreeCycleAndFrontmatterLimit(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, ".agents", "skills")
	skill := skillFixture(t, parent, "loop")
	directoryLink(t, skill, filepath.Join(skill, "loop"))
	config := loadOK(t, root)
	if len(config.Skills) != 0 {
		t.Fatal("skill directory cycle accepted")
	}
	expectWarnings(t, config, "cycle")
	oversized := "---\nname: large\ndescription: |\n  " + strings.Repeat("x", maxFrontmatterBytes) + "\n---\n"
	if _, err := validateSkill([]byte(oversized), "large"); err == nil {
		t.Fatal("oversized frontmatter accepted")
	}
}

func TestWindowsJunctionEscape(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows junctions only")
	}
	root := t.TempDir()
	outside := t.TempDir()
	plugin := pluginFixture(t, root, "demo", "demo", testObject{
		"bad":  testObject{"type": "stdio", "command": "node", "cwd": "./junction"},
		"good": testObject{"type": "stdio", "command": "node"},
	})
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", filepath.Join(plugin, "junction"), outside).CombinedOutput()
	if err != nil {
		t.Skipf("Windows junction unavailable: %v: %s", err, output)
	}
	config := loadOK(t, root)
	if !reflect.DeepEqual(serverNames(config), []string{"demo.good"}) {
		t.Fatal(config.Servers)
	}
	expectWarnings(t, config, "escapes permitted root")
}

func TestCheckedInExamplesAreInactiveAndValidWhenCopied(t *testing.T) {
	examples := filepath.Join("..", "..", "examples")
	inactive := loadOK(t, examples)
	if len(inactive.Servers) != 0 || len(inactive.Skills) != 0 || len(inactive.Warnings) != 0 {
		t.Fatal("examples unexpectedly activate themselves")
	}
	root := t.TempDir()
	files := map[string]string{
		"canonical/mcp.json.example":                              ".agents/mcp.json",
		"canonical/agentmux.json.example":                         ".agents/agentmux.json",
		"plugins/docs-helper/plugin.json":                         ".agents/plugins/docs-helper/plugin.json",
		"plugins/docs-helper/mcp.json":                            ".agents/plugins/docs-helper/mcp.json",
		"plugins/docs-helper/skills/documentation-check/SKILL.md": ".agents/plugins/docs-helper/skills/documentation-check/SKILL.md",
		"skills/quick-review/SKILL.md":                            ".agents/skills/quick-review/SKILL.md",
	}
	for source, destination := range files {
		data, err := os.ReadFile(filepath.Join(examples, filepath.FromSlash(source)))
		if err != nil {
			t.Fatal(err)
		}
		putFile(t, filepath.Join(root, filepath.FromSlash(destination)), string(data))
	}
	config := loadOK(t, root)
	if !reflect.DeepEqual(serverNames(config), []string{"chrome-devtools", "context7", "docs-helper.context7"}) {
		t.Fatal(config.Servers)
	}
	if !reflect.DeepEqual(skillNames(config), []string{"docs-helper.documentation-check", "quick-review"}) {
		t.Fatal(config.Skills)
	}
	if len(config.Warnings) != 0 {
		t.Fatal(config.Warnings)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents", "data")); !os.IsNotExist(err) {
		t.Fatal("example loading created runtime data")
	}
}
