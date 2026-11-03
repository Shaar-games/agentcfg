package config

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMCPHarnessFilterParsing(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		want []string
	}{
		{name: "absent", want: nil},
		{name: "all", raw: json.RawMessage(`[]`), want: []string{}},
		{name: "none", raw: json.RawMessage(`["none"]`), want: []string{"none"}},
		{name: "named", raw: json.RawMessage(`["codex","cursor","codex","antigravity"]`), want: []string{"codex", "cursor", "antigravity"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseServerHarnesses(tc.raw)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("filter = %#v, error = %v; want %#v", got, err, tc.want)
			}
		})
	}
	for _, raw := range []string{
		`["none","codex"]`, `["codex","none"]`, `["none","none"]`,
		`["None"]`, `[" none"]`, `["none "]`, `["unknown"]`,
		`null`, `"none"`, `[null]`, `["none",1]`, `{}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseServerHarnesses(json.RawMessage(raw)); err == nil {
				t.Fatalf("accepted invalid MCP harness filter %s", raw)
			}
		})
	}
}

func TestMCPNoneCanonicalAndOverrideSelections(t *testing.T) {
	home := t.TempDir()
	canonicalServers(t, home, testObject{
		"canonical-none":  testObject{"command": "never-executed", "harnesses": []string{"none"}},
		"http-none":       testObject{"url": "https://example.test/mcp", "harnesses": []string{"none"}},
		"override-none":   testObject{"command": "never-executed", "harnesses": []string{"codex"}},
		"override-empty":  testObject{"command": "never-executed", "harnesses": []string{"none"}},
		"override-absent": testObject{"command": "never-executed", "harnesses": []string{"none"}},
		"unfiltered":      testObject{"command": "never-executed"},
	})
	putJSON(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), testObject{
		"harnesses": []string{"codex", "cursor"},
		"serverOverrides": testObject{
			"override-none":   testObject{"harnesses": []string{"none"}},
			"override-empty":  testObject{"harnesses": []string{}},
			"override-absent": testObject{"sharing": "isolated"},
		},
	})
	cfg := loadOK(t, home)
	if len(cfg.Warnings) != 0 || len(cfg.Servers) != 6 {
		t.Fatalf("servers = %v; warnings = %v", serverNames(cfg), cfg.Warnings)
	}
	for _, name := range []string{"canonical-none", "http-none", "override-none", "override-absent"} {
		filter := cfg.Servers[name].Harnesses
		if !reflect.DeepEqual(filter, []string{"none"}) {
			t.Errorf("%s lost explicit none: %#v", name, filter)
		}
		for _, harness := range cfg.Settings.Harnesses {
			if Allows(filter, harness) {
				t.Errorf("%s is enabled for %s", name, harness)
			}
		}
		if !Allows(filter, "") {
			t.Errorf("%s changed direct connection behavior", name)
		}
	}
	if cfg.Settings.ServerOverrides["override-absent"].Harnesses != nil {
		t.Fatal("absent override became an explicit selection")
	}
	if filter := cfg.Settings.ServerOverrides["override-empty"].Harnesses; filter == nil || len(filter) != 0 {
		t.Fatalf("explicit empty override lost its distinction from absent: %#v", filter)
	}
	filter := cfg.Servers["override-empty"].Harnesses
	if filter == nil || len(filter) != 0 || !Allows(filter, "codex") || !Allows(filter, "cursor") {
		t.Fatalf("empty override failed to reset none to all: %#v", filter)
	}
	if cfg.Servers["unfiltered"].Harnesses != nil {
		t.Fatal("absent canonical filter changed representation")
	}
}

func TestMCPNoneKeepsPortablePluginsStrict(t *testing.T) {
	home := t.TempDir()
	pluginFixture(t, home, "demo", "demo", testObject{
		"valid":   testObject{"type": "streamable-http", "url": "https://example.test/mcp"},
		"invalid": testObject{"type": "stdio", "command": "never-executed", "harnesses": []string{"none"}},
	})
	putJSON(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), testObject{
		"serverOverrides": testObject{"demo.valid": testObject{"harnesses": []string{"none"}}},
	})
	cfg := loadOK(t, home)
	if !reflect.DeepEqual(serverNames(cfg), []string{"demo.valid"}) {
		t.Fatalf("portable filter accepted or valid sibling lost: %v", serverNames(cfg))
	}
	if !reflect.DeepEqual(cfg.Servers["demo.valid"].Harnesses, []string{"none"}) {
		t.Fatal("external plugin override did not accept none")
	}
	expectWarnings(t, cfg, "demo.invalid", "harnesses", "serverOverrides")
}

func TestInvalidMCPNoneFiltersRespectIsolationBoundaries(t *testing.T) {
	home := t.TempDir()
	canonicalServers(t, home, testObject{
		"valid":   testObject{"command": "never-executed", "harnesses": []string{"none"}},
		"invalid": testObject{"command": "never-executed", "harnesses": []string{"codex", "none"}},
	})
	cfg := loadOK(t, home)
	if !reflect.DeepEqual(serverNames(cfg), []string{"valid"}) {
		t.Fatalf("invalid canonical filter was not isolated: %v", serverNames(cfg))
	}
	expectWarnings(t, cfg, "invalid", "none must be the only entry")
	putJSON(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), testObject{
		"serverOverrides": testObject{"valid": testObject{"harnesses": []string{"none", "cursor"}}},
	})
	if _, err := Load(home); err == nil {
		t.Fatal("invalid settings override was silently accepted")
	}
}

func TestNoneIsNotAConfiguredUserHarness(t *testing.T) {
	for _, selection := range [][]string{{"none"}, {"none", "codex"}, {"codex", "none"}} {
		if _, err := ValidateUserHarnesses(selection); err == nil {
			t.Errorf("ValidateUserHarnesses accepted %v", selection)
		}
		home := t.TempDir()
		putJSON(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), testObject{"harnesses": selection})
		if _, err := Load(home); err == nil {
			t.Errorf("Settings.Harnesses accepted %v", selection)
		}
	}
}

func TestAllowsNoneAndDirectConnection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter []string
		codex  bool
		cursor bool
	}{
		{name: "absent", codex: true, cursor: true},
		{name: "empty", filter: []string{}, codex: true, cursor: true},
		{name: "named", filter: []string{"codex"}, codex: true},
		{name: "none", filter: []string{"none"}},
		{name: "invalid-none-first", filter: []string{"none", "codex"}},
		{name: "invalid-none-last", filter: []string{"codex", "none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if Allows(tc.filter, "codex") != tc.codex || Allows(tc.filter, "cursor") != tc.cursor {
				t.Fatalf("unexpected named routing for %#v", tc.filter)
			}
			if !Allows(tc.filter, "") {
				t.Fatalf("direct connection was filtered for %#v", tc.filter)
			}
		})
	}
	for _, harness := range []string{"claude-code", "codex", "cursor", "opencode", "gemini", "antigravity", "vscode", "none", "unknown"} {
		if Allows([]string{"none"}, harness) {
			t.Errorf("none enabled named harness %q", harness)
		}
	}
}
