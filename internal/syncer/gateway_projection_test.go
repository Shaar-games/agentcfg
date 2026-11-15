package syncer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"
	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
)

const gatewayFixtureServer = "remote / café"

func initializeGatewayFixture(t *testing.T, root string) {
	t.Helper()
	t.Setenv("AGENTCFG_OAUTH_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32)))
	if _, err := auth.GatewayKey(t.Context(), root, nil, true); err != nil {
		t.Fatal(err)
	}
}

func gatewaySyncFixture(t *testing.T, harness string) (*config.Config, Options, target) {
	t.Helper()
	home, err := pathutil.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	privateGatewayFixtureHome(t, home)
	writeTestFile(t, filepath.Join(home, ".agentcfg", "mcp.json"), `{"mcpServers":{
		"remote / café":{"url":"https://upstream.invalid/mcp?key=upstream-secret","headers":{"Authorization":"Bearer upstream-secret"}},
		"local":{"command":"inert-fixture-command"},
		"cursor-only":{"url":"https://upstream.invalid/cursor","harnesses":["cursor"]},
		"disabled":{"url":"https://upstream.invalid/disabled","harnesses":["none"]}
	}}`)
	settings, err := json.Marshal(map[string]any{"harnesses": []string{harness}})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), string(settings))
	var native target
	for _, candidate := range registry(home, "user") {
		if candidate.name == harness {
			native = candidate
		}
	}
	if native.path == "" {
		t.Fatal("missing fixture target")
	}
	if harness == "opencode" {
		native.path += "c"
	}
	personal := map[string]any{"command": "keep-personal"}
	if harness == "opencode" {
		personal = map[string]any{"type": "local", "command": []string{"keep-personal"}}
	}
	nativeData, err := json.Marshal(map[string]any{"theme": "untouched", native.key: map[string]any{"personal": personal}})
	if err != nil {
		t.Fatal(err)
	}
	nativeData = append([]byte("// Keep this native comment.\n"), nativeData...)
	if native.toml {
		nativeData = []byte("# Keep this native comment.\nmodel = 'untouched'\n[mcp_servers.personal]\ncommand = 'keep-personal'\n")
	}
	writeTestFile(t, native.path, string(nativeData))
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, Options{Root: home, Home: home, Scope: "user", Binary: filepath.Join(home, "bin", "agentcfg.exe"), SkillMode: "copy"}, native
}

func gatewayNativeEntries(t *testing.T, native target) (map[string]any, []byte) {
	t.Helper()
	data, err := os.ReadFile(native.path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if native.toml {
		err = toml.Unmarshal(data, &document)
	} else {
		var standard []byte
		standard, err = hujson.Standardize(append([]byte(nil), data...))
		if err == nil {
			err = json.Unmarshal(standard, &document)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	entries, ok := document[native.key].(map[string]any)
	if !ok {
		t.Fatalf("missing native MCP section: %s", data)
	}
	return entries, data
}

func gatewayFixtureSettings(t *testing.T, cfg *config.Config, enabled bool) *config.Config {
	t.Helper()
	settings := cfg.Settings
	settings.Gateway.Enabled = enabled
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(cfg.Root, ".agentcfg", "agentcfg.json"), string(data))
	if enabled {
		initializeGatewayFixture(t, cfg.Root)
	}
	updated, err := config.Load(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func applyGatewayFixture(t *testing.T, cfg *config.Config, opts Options) {
	t.Helper()
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayProjectionMixesHTTPAndStdioHarnesses(t *testing.T) {
	cfg, opts, cursor := gatewaySyncFixture(t, "cursor")
	cfg.Settings.Harnesses = []string{"cursor", "opencode"}
	cfg.Settings.Gateway.Harnesses = []string{"cursor"}
	cfg = gatewayFixtureSettings(t, cfg, true)
	canonicalPath := filepath.Join(cfg.Root, ".agentcfg", "mcp.json")
	canonicalBefore, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	applyGatewayFixture(t, cfg, opts)
	cursorEntries, _ := gatewayNativeEntries(t, cursor)
	if got := cursorEntries[gatewayFixtureServer].(map[string]any)["url"]; got != config.GatewayResourceURL(cfg.Settings.Gateway, gatewayFixtureServer, "cursor") {
		t.Fatal("selected Cursor harness did not receive its native HTTP route")
	}
	for _, native := range registry(cfg.Root, "user") {
		if native.name != "opencode" {
			continue
		}
		entries, _ := gatewayNativeEntries(t, native)
		if digest(entries[gatewayFixtureServer]) != digest(proxyEntry(native, opts.Binary, cfg.Root, gatewayFixtureServer)) {
			t.Fatal("excluded OpenCode harness did not retain its stdio connect proxy")
		}
		if _, exists := entries["cursor-only"]; exists {
			t.Fatal("gateway selection bypassed the server harness filter")
		}
	}
	if digest(cursorEntries["local"]) != digest(proxyEntry(cursor, opts.Binary, cfg.Root, "local")) {
		t.Fatal("gateway selection changed a local stdio MCP")
	}
	assertAdoptionBytes(t, canonicalPath, canonicalBefore)
}

func TestGatewayProjectionMigratesEachNativeFormat(t *testing.T) {
	for _, harness := range []string{"claude-code", "cursor", "codex", "opencode", "gemini", "antigravity"} {
		t.Run(harness, func(t *testing.T) {
			cfg, opts, native := gatewaySyncFixture(t, harness)
			canonicalPath := filepath.Join(cfg.Root, ".agentcfg", "mcp.json")
			canonicalBefore, err := os.ReadFile(canonicalPath)
			if err != nil {
				t.Fatal(err)
			}
			personalBefore, _ := gatewayNativeEntries(t, native)
			applyGatewayFixture(t, cfg, opts)
			entries, nativeBefore := gatewayNativeEntries(t, native)
			remote := entries[gatewayFixtureServer].(map[string]any)
			if remote["command"] == nil || remote["url"] != nil || remote["httpUrl"] != nil || remote["serverUrl"] != nil {
				t.Fatalf("gateway was activated without an explicit setting: %#v", remote)
			}
			cfg = gatewayFixtureSettings(t, cfg, true)
			plan, err := BuildPlan(cfg, opts)
			if err != nil {
				t.Fatal(err)
			}
			assertAdoptionBytes(t, native.path, nativeBefore)
			if len(plan.Changes) != 1 || plan.Changes[0].Path != native.path {
				t.Fatalf("unexpected gateway migration changes: %+v", plan.Changes)
			}
			if err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			entries, projected := gatewayNativeEntries(t, native)
			url := config.GatewayResourceURL(cfg.Settings.Gateway, gatewayFixtureServer, harness)
			want := map[string]any{"url": url}
			switch harness {
			case "claude-code":
				want["type"] = "http"
			case "opencode":
				want["type"], want["enabled"] = "remote", true
			case "gemini":
				want = map[string]any{"httpUrl": url}
			case "antigravity":
				want = map[string]any{"serverUrl": url}
			}
			if !reflect.DeepEqual(entries[gatewayFixtureServer], want) {
				t.Fatalf("projected remote = %#v, want %#v", entries[gatewayFixtureServer], want)
			}
			if entries["local"].(map[string]any)["command"] == nil {
				t.Fatal("stdio upstream stopped using the connect proxy")
			}
			if !reflect.DeepEqual(entries["personal"], personalBefore["personal"]) || !bytes.Contains(projected, []byte("Keep this native comment.")) {
				t.Fatal("unmanaged native content was changed")
			}
			if entries["disabled"] != nil || (entries["cursor-only"] != nil) != (harness == "cursor") {
				t.Fatal("gateway projection changed canonical harness filtering")
			}
			if bytes.Contains(projected, []byte("upstream-secret")) || bytes.Contains(projected, []byte("upstream.invalid")) {
				t.Fatal("upstream URL or credentials leaked into the native gateway entry")
			}
			assertAdoptionBytes(t, canonicalPath, canonicalBefore)
			again, err := BuildPlan(cfg, opts)
			if err != nil || len(again.Changes) != 0 {
				t.Fatalf("gateway sync is not idempotent: %v, %+v", err, again)
			}
			cfg = gatewayFixtureSettings(t, cfg, false)
			applyGatewayFixture(t, cfg, opts)
			entries, _ = gatewayNativeEntries(t, native)
			if entries[gatewayFixtureServer].(map[string]any)["command"] == nil {
				t.Fatal("explicitly disabling the gateway did not restore stdio projection")
			}
			assertAdoptionBytes(t, canonicalPath, canonicalBefore)
		})
	}
}

func TestGatewayProjectionRefusesManualDrift(t *testing.T) {
	for _, afterMigration := range []bool{false, true} {
		t.Run(map[bool]string{false: "stdio-before-enable", true: "http-before-disable"}[afterMigration], func(t *testing.T) {
			cfg, opts, native := gatewaySyncFixture(t, "cursor")
			cfg = gatewayFixtureSettings(t, cfg, afterMigration)
			applyGatewayFixture(t, cfg, opts)
			_, data := gatewayNativeEntries(t, native)
			edited := strings.ReplaceAll(string(data), "connect", "manual-connect")
			if afterMigration {
				edited = strings.ReplaceAll(string(data), "127.0.0.1:43877", "127.0.0.1:43878")
			}
			if edited == string(data) {
				t.Fatal("fixture did not modify a managed entry")
			}
			writeTestFile(t, native.path, edited)
			cfg = gatewayFixtureSettings(t, cfg, !afterMigration)
			if _, err := BuildPlan(cfg, opts); err == nil || !strings.Contains(err.Error(), "manually") {
				t.Fatalf("gateway migration accepted managed drift: %v", err)
			}
			assertAdoptionBytes(t, native.path, []byte(edited))
		})
	}
}

func TestGatewayProjectionKeepsInitRestoreBaseline(t *testing.T) {
	f := newAdoptionFixture(t, "cursor", false)
	home := f.opts.Home
	privateGatewayFixtureHome(t, home)
	if err := CreateInitRestorePoint(home); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeInitLinks(context.Background(), home, []string{"cursor"}, nil); err != nil {
		t.Fatal(err)
	}
	applyGatewayFixture(t, f.cfg, f.opts)
	writeTestFile(t, filepath.Join(home, ".agentcfg", "mcp.json"), `{"mcpServers":{"echo":{"command":"node","args":["server.js"]},"remote":{"url":"https://example.invalid/mcp"}}}`)
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg = gatewayFixtureSettings(t, cfg, true)
	opts := f.opts
	opts.AdoptExisting, opts.AdoptMCP, opts.AdoptSkills = false, nil, nil
	applyGatewayFixture(t, cfg, opts)
	entries, _ := gatewayNativeEntries(t, f.target)
	if entries["remote"].(map[string]any)["url"] != config.GatewayResourceURL(cfg.Settings.Gateway, "remote", "cursor") {
		t.Fatal("new HTTP entry was not installed after init")
	}
	restore, err := BuildRestorePlan(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.Apply(); err != nil {
		t.Fatal(err)
	}
	assertAdoptionBytes(t, f.target.path, f.before)
}

func TestGatewayProjectionAcceptsHTTPAliasAndTLS(t *testing.T) {
	cfg, opts, native := gatewaySyncFixture(t, "cursor")
	initializeGatewayFixture(t, cfg.Root)
	cfg.Settings.Gateway = config.GatewaySettings{Enabled: true, Listen: "[::1]:43878", TLSCert: "fixture.crt", TLSKey: "fixture.key"}
	entry := cfg.Servers[gatewayFixtureServer]
	entry.Type = "http"
	cfg.Servers[gatewayFixtureServer] = entry
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	var projected []byte
	for _, change := range plan.Changes {
		if change.Path == native.path {
			projected = change.after
		}
	}
	if !bytes.Contains(projected, []byte("https://[::1]:43878/mcp/")) {
		t.Fatalf("HTTP alias/TLS projection is missing: %s", projected)
	}
}

func TestGatewayProjectionRequiresInitializedKeyWithoutWriting(t *testing.T) {
	cfg, opts, native := gatewaySyncFixture(t, "cursor")
	t.Setenv("AGENTCFG_OAUTH_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32)))
	cfg.Settings.Gateway.Enabled = true
	_, before := gatewayNativeEntries(t, native)
	if _, err := BuildPlan(cfg, opts); !errors.Is(err, auth.ErrGatewayNotInitialized) {
		t.Fatalf("uninitialized projection: %v", err)
	}
	if _, err := auth.GatewayKey(t.Context(), cfg.Root, nil, false); !errors.Is(err, auth.ErrGatewayNotInitialized) {
		t.Fatalf("planning initialized routing credentials: %v", err)
	}
	assertAdoptionBytes(t, native.path, before)
	if _, err := os.Lstat(filepath.Join(cfg.Root, ".agentcfg", "sync-user.json")); !os.IsNotExist(err) {
		t.Fatal("failed preview created an ownership manifest")
	}
	if _, err := os.Lstat(filepath.Join(cfg.Root, ".agentcfg", "credentials")); !os.IsNotExist(err) {
		t.Fatal("read-only projection created a credential directory")
	}
}

func TestGatewayDisabledProjectionDoesNotOpenCredentialStore(t *testing.T) {
	cfg, opts, _ := gatewaySyncFixture(t, "cursor")
	t.Setenv("AGENTCFG_OAUTH_KEY", "not-a-valid-encryption-key")
	if _, err := BuildPlan(cfg, opts); err != nil {
		t.Fatalf("ordinary stdio projection inspected OAuth credentials: %v", err)
	}
}
