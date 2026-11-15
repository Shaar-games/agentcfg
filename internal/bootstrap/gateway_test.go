package bootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"
	"agentcfg/internal/syncer"
)

func gatewayDiscoveryFixture(t *testing.T, harness string) (string, string, *config.Config) {
	t.Helper()
	home, err := pathutil.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	privateGatewayFixtureHome(t, home)
	put(t, home, ".agentcfg/mcp.json", `{"mcpServers":{"remote":{"url":"https://upstream.invalid/mcp"}}}`)
	settings, err := json.Marshal(map[string]any{"harnesses": []string{harness}, "gateway": map[string]any{"enabled": true}})
	if err != nil {
		t.Fatal(err)
	}
	put(t, home, ".agentcfg/agentcfg.json", string(settings))
	t.Setenv("AGENTCFG_OAUTH_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5b}, 32)))
	if _, err := auth.GatewayKey(t.Context(), home, nil, true); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Scope: "user", Binary: filepath.Join(home, "bin", "agentcfg"), SkillMode: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if target.name == harness {
			return home, filepath.Join(home, filepath.FromSlash(target.configs[0])), cfg
		}
	}
	t.Fatal("unknown fixture harness")
	return "", "", nil
}

func TestDiscoverSkipsOwnedGatewayProxies(t *testing.T) {
	for _, harness := range []string{"claude-code", "cursor", "codex", "opencode", "gemini", "antigravity"} {
		t.Run(harness, func(t *testing.T) {
			home, native, _ := gatewayDiscoveryFixture(t, harness)
			before := map[string][]byte{}
			for _, path := range []string{native, filepath.Join(home, ".agentcfg", "mcp.json"), filepath.Join(home, ".agentcfg", "agentcfg.json"), filepath.Join(home, ".agentcfg", "sync-user.json")} {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = data
			}
			plan, err := Discover(home)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Servers) != 0 {
				t.Fatalf("managed gateway was imported as its own upstream: %+v", plan.Servers)
			}
			found := false
			for _, warning := range plan.Warnings {
				if warning.Server == "remote" && warning.Harness == harness && warning.Kind == WarningAdvisory && strings.Contains(warning.Message, "agentcfg HTTP gateway proxy") {
					found = true
				}
			}
			if !found {
				t.Fatalf("managed gateway provenance was not reported: %+v", plan.Warnings)
			}
			again, err := Discover(home)
			if err != nil || !reflect.DeepEqual(plan, again) {
				t.Fatalf("gateway discovery is not deterministic: %v", err)
			}
			for path, want := range before {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("gateway discovery changed %s", path)
				}
			}
		})
	}
}

func TestDiscoverSkipsOwnedGatewayAfterCanonicalRemoval(t *testing.T) {
	home, native, _ := gatewayDiscoveryFixture(t, "cursor")
	before, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	put(t, home, ".agentcfg/mcp.json", `{"mcpServers":{}}`)
	plan, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Servers) != 0 {
		t.Fatal("an intact owned gateway route was reimported after canonical removal")
	}
	found := false
	for _, warning := range plan.Warnings {
		found = found || warning.Server == "remote" && warning.Harness == "cursor" && warning.Kind == WarningAdvisory && strings.Contains(warning.Message, "agentcfg HTTP gateway proxy")
	}
	if !found {
		t.Fatal("removed canonical server lost the intact route ownership advisory")
	}
	after, err := os.ReadFile(native)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("discovery changed the native gateway route")
	}
}

func TestDiscoverGatewayProvenanceDoesNotHideManualLocalMCPs(t *testing.T) {
	for _, kind := range []string{"unowned-route", "ordinary-localhost", "other-origin", "other-route", "other-harness", "query", "headers"} {
		t.Run(kind, func(t *testing.T) {
			home, native, cfg := gatewayDiscoveryFixture(t, "cursor")
			data, err := os.ReadFile(native)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]map[string]map[string]any
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			entry := document["mcpServers"]["remote"]
			switch kind {
			case "unowned-route":
				if err := os.Remove(filepath.Join(home, ".agentcfg", "sync-user.json")); err != nil {
					t.Fatal(err)
				}
			case "ordinary-localhost":
				entry["url"] = "http://127.0.0.1:51234/my-mcp"
			case "other-origin":
				entry["url"] = strings.Replace(entry["url"].(string), ":43877/", ":43878/", 1)
			case "other-route":
				entry["url"] = config.GatewayBaseURL(cfg.Settings.Gateway) + "/mcp/manual"
			case "other-harness":
				entry["url"] = config.GatewayResourceURL(cfg.Settings.Gateway, "remote", "codex")
			case "query":
				entry["url"] = entry["url"].(string) + "?manual=value"
			case "headers":
				entry["headers"] = map[string]string{"X-Manual": "keep-this-value"}
			}
			updated, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			put(t, home, ".cursor/mcp.json", string(updated))
			plan, err := Discover(home)
			if err != nil || len(plan.Servers) != 1 || plan.Servers[0].Server.URL != entry["url"] {
				t.Fatalf("manual local endpoint was hidden: plan=%+v err=%v", plan, err)
			}
			if kind == "headers" && plan.Servers[0].Server.Headers["X-Manual"] != "keep-this-value" {
				t.Fatal("manual header was lost")
			}
		})
	}
}

func TestDiscoverGatewayOwnershipIsBoundToTheSourceRoot(t *testing.T) {
	original, _, _ := gatewayDiscoveryFixture(t, "cursor")
	other := t.TempDir()
	for _, relative := range []string{".agentcfg/mcp.json", ".agentcfg/agentcfg.json", ".agentcfg/sync-user.json", ".cursor/mcp.json"} {
		data, err := os.ReadFile(filepath.Join(original, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		put(t, other, relative, string(data))
	}
	plan, err := Discover(other)
	if err != nil || len(plan.Servers) != 1 {
		t.Fatalf("ownership from another root hid a local endpoint: plan=%+v err=%v", plan, err)
	}
}

func TestDiscoverRecognizesOwnedRouteAfterGatewayIsDisabled(t *testing.T) {
	home, _, cfg := gatewayDiscoveryFixture(t, "cursor")
	cfg.Settings.Gateway.Enabled = false
	settings, err := json.Marshal(cfg.Settings)
	if err != nil {
		t.Fatal(err)
	}
	put(t, home, ".agentcfg/agentcfg.json", string(settings))
	plan, err := Discover(home)
	if err != nil || len(plan.Servers) != 0 {
		t.Fatalf("owned route was reimported before the disabling sync: plan=%+v err=%v", plan, err)
	}
}

func TestDiscoverOwnedGatewayWithUnavailableKeyBlocksImportWithoutWrites(t *testing.T) {
	for _, reason := range []string{"missing", "unavailable", "changed"} {
		t.Run(reason, func(t *testing.T) {
			home, native, _ := gatewayDiscoveryFixture(t, "cursor")
			before, err := os.ReadFile(native)
			if err != nil {
				t.Fatal(err)
			}
			credentials := filepath.Join(home, ".agentcfg", "credentials")
			if reason == "unavailable" {
				t.Setenv("AGENTCFG_OAUTH_KEY", "invalid-test-key")
			} else {
				if err := os.RemoveAll(credentials); err != nil {
					t.Fatal(err)
				}
				if reason == "changed" {
					if _, err := auth.GatewayKey(t.Context(), home, nil, true); err != nil {
						t.Fatal(err)
					}
				}
			}
			plan, err := Discover(home)
			if err != nil || len(plan.Servers) != 0 {
				t.Fatalf("unverifiable owned route became an upstream: plan=%+v err=%v", plan, err)
			}
			blocked := false
			for _, warning := range plan.Warnings {
				if warning.Server == "remote" && warning.BlocksInit([]string{"cursor"}) && strings.Contains(warning.Message, "gateway route") {
					blocked = true
				}
			}
			if !blocked {
				t.Fatalf("missing ownership verification was hidden: %+v", plan.Warnings)
			}
			after, err := os.ReadFile(native)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("discovery changed the native capability URL")
			}
			if reason == "missing" {
				if _, err := os.Lstat(credentials); !os.IsNotExist(err) {
					t.Fatal("discovery initialized missing routing credentials")
				}
			}
		})
	}
}

func TestDiscoverOrdinaryOwnedHTTPDoesNotOpenGatewayStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTCFG_OAUTH_KEY", "invalid-test-key")
	put(t, home, ".agentcfg/mcp.json", `{"mcpServers":{"remote":{"url":"https://upstream.invalid/mcp"}}}`)
	put(t, home, ".agentcfg/agentcfg.json", `{"harnesses":["cursor"]}`)
	entry := map[string]any{"url": "http://127.0.0.1:43877/mcp/ordinary"}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(home, ".cursor", "mcp.json")
	put(t, home, ".cursor/mcp.json", `{"mcpServers":{"remote":`+string(encoded)+`}}`)
	physicalHome, err := pathutil.Resolve(home)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{"version": 1, "configs": map[string]any{
		filepath.Join(physicalHome, ".cursor", "mcp.json"): map[string]string{"remote": fmt.Sprintf("%x", sha256.Sum256(encoded))},
	}})
	if err != nil {
		t.Fatal(err)
	}
	put(t, home, ".agentcfg/sync-user.json", string(manifest))
	plan, err := Discover(home)
	if err != nil || len(plan.Servers) != 1 || plan.Servers[0].Server.URL != entry["url"] {
		t.Fatalf("ordinary local MCP required gateway credentials: plan=%+v err=%v", plan, err)
	}
	for _, warning := range plan.Warnings {
		if warning.BlocksInit([]string{"cursor"}) || strings.Contains(warning.Message, "gateway route") {
			t.Fatalf("unused gateway credential store affected discovery: %+v", plan.Warnings)
		}
	}
	if _, err := os.Stat(native); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".agentcfg", "credentials")); !os.IsNotExist(err) {
		t.Fatal("ordinary discovery created a credential store")
	}
}
