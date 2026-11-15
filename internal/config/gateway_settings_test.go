package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGatewaySettingsDefaultsAndAddresses(t *testing.T) {
	for _, test := range []struct {
		name, data, listen string
		enabled            bool
	}{
		{name: "missing settings", listen: DefaultGatewayListen},
		{name: "omitted", data: `{}`, listen: DefaultGatewayListen},
		{name: "empty", data: `{"gateway":{}}`, listen: DefaultGatewayListen},
		{name: "disabled", data: `{"gateway":{"enabled":false}}`, listen: DefaultGatewayListen},
		{name: "enabled", data: `{"gateway":{"enabled":true}}`, listen: DefaultGatewayListen, enabled: true},
		{name: "IPv4", data: `{"gateway":{"enabled":true,"listen":"127.2.3.4:49876"}}`, listen: "127.2.3.4:49876", enabled: true},
		{name: "IPv6", data: `{"gateway":{"listen":"[0:0:0:0:0:0:0:1]:43877"}}`, listen: "[::1]:43877"},
		{name: "normalized port", data: `{"gateway":{"listen":"127.0.0.1:043877"}}`, listen: DefaultGatewayListen},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			if test.data != "" {
				putFile(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), test.data)
			}
			cfg := loadOK(t, home)
			want := GatewaySettings{Enabled: test.enabled, Listen: test.listen}
			if !reflect.DeepEqual(cfg.Settings.Gateway, want) {
				t.Fatalf("gateway = %+v, want %+v", cfg.Settings.Gateway, want)
			}
			if len(cfg.Warnings) != 0 {
				t.Fatalf("valid settings produced warnings: %v", cfg.Warnings)
			}
			if test.data != "" {
				data, err := os.ReadFile(filepath.Join(home, ".agentcfg", "agentcfg.json"))
				if err != nil || string(data) != test.data {
					t.Fatal("loading gateway settings changed the source")
				}
			}
		})
	}
}

func TestGatewaySettingsRejectInvalidValues(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `true`, `1`, `"gateway"`,
		`{"enabled":null}`, `{"enabled":"true"}`, `{"enabled":1}`, `{"enabled":[]}`,
		`{"unknown":true}`, `{"Enabled":true}`, `{"enabled":true,"enabled":false}`,
		`{"routeKey":null}`, `{"routeKey":"not-a-canonical-secret"}`,
		`{"harnesses":null}`, `{"harnesses":"cursor"}`, `{"harnesses":true}`, `{"harnesses":1}`,
		`{"harnesses":[null]}`, `{"harnesses":[1]}`, `{"harnesses":["unknown"]}`, `{"harnesses":["none","cursor"]}`,
		`{"listen":null}`, `{"listen":1}`, `{"listen":""}`, `{"listen":" "}`,
		`{"listen":"localhost:43877"}`, `{"listen":"0.0.0.0:43877"}`, `{"listen":"192.168.1.1:43877"}`,
		`{"listen":"[::]:43877"}`, `{"listen":"[2001:db8::1]:43877"}`, `{"listen":"[::1%lo]:43877"}`,
		`{"listen":"127.0.0.1"}`, `{"listen":"127.0.0.1:0"}`, `{"listen":"127.0.0.1:65536"}`,
		`{"listen":"127.0.0.1:-1"}`, `{"listen":"127.0.0.1:+1"}`, `{"listen":"127.0.0.1:http"}`,
		`{"listen":"http://127.0.0.1:43877"}`, `{"listen":"127.0.0.1:43877/mcp"}`,
		`{"tlsCert":"certificate.pem"}`, `{"tlsKey":"key.pem"}`,
		`{"tlsCert":"","tlsKey":"key.pem"}`, `{"tlsCert":"certificate.pem","tlsKey":""}`,
		`{"tlsCert":null,"tlsKey":"key.pem"}`, `{"tlsCert":"certificate.pem","tlsKey":false}`,
		`{"tlsCert":"missing.pem","tlsKey":"key.pem"}`, `{"tlsCert":"directory","tlsKey":"key.pem"}`,
		`{"tlsCert":"certificate.pem","tlsKey":"line\nkey.pem"}`, `{"tlsCert":"certificate.pem","tlsKey":"\u0000"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			home := t.TempDir()
			putFile(t, filepath.Join(home, "certificate.pem"), "fixture certificate")
			putFile(t, filepath.Join(home, "key.pem"), "fixture key")
			if err := os.Mkdir(filepath.Join(home, "directory"), 0700); err != nil {
				t.Fatal(err)
			}
			putFile(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), `{"gateway":`+raw+`}`)
			if _, err := Load(home); err == nil {
				t.Fatal("invalid gateway settings were accepted")
			}
		})
	}
}

func TestGatewayHarnessSelection(t *testing.T) {
	for _, test := range []struct {
		name, data               string
		harnesses                []string
		cursor, opencode, direct bool
	}{
		{name: "disabled", data: `{"enabled":false,"harnesses":["cursor"]}`, harnesses: []string{"cursor"}},
		{name: "omitted", data: `{"enabled":true}`, cursor: true, opencode: true, direct: true},
		{name: "empty", data: `{"enabled":true,"harnesses":[]}`, cursor: true, opencode: true, direct: true},
		{name: "cursor only", data: `{"enabled":true,"harnesses":["cursor"]}`, harnesses: []string{"cursor"}, cursor: true, direct: true},
		{name: "none", data: `{"enabled":true,"harnesses":["none"]}`, harnesses: []string{"none"}, direct: true},
		{name: "deduplicated", data: `{"enabled":true,"harnesses":["cursor","cursor"]}`, harnesses: []string{"cursor"}, cursor: true, direct: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			putFile(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), `{"gateway":`+test.data+`}`)
			settings := loadOK(t, home).Settings.Gateway
			if len(settings.Harnesses) != len(test.harnesses) || len(test.harnesses) != 0 && !reflect.DeepEqual(settings.Harnesses, test.harnesses) {
				t.Fatalf("gateway harness filter = %v, want %v", settings.Harnesses, test.harnesses)
			}
			for name, want := range map[string]bool{"cursor": test.cursor, "opencode": test.opencode, "": test.direct} {
				if got := GatewayForHarness(settings, name); got != want {
					t.Errorf("gateway eligibility for %q = %v, want %v", name, got, want)
				}
			}
		})
	}
}

func TestGatewaySettingsTLSPathsAndRoundTrip(t *testing.T) {
	for _, absolute := range []bool{false, true} {
		t.Run(map[bool]string{false: "relative", true: "absolute"}[absolute], func(t *testing.T) {
			home := t.TempDir()
			cert, key := filepath.Join("tls", "certificate.pem"), filepath.Join("tls", "key.pem")
			putFile(t, filepath.Join(home, cert), "not read as PEM while loading settings")
			putFile(t, filepath.Join(home, key), "not read as a key while loading settings")
			if absolute {
				cert, key = filepath.Join(home, cert), filepath.Join(home, key)
			}
			settings := GatewaySettings{Enabled: true, Listen: "[::1]:49876", TLSCert: cert, TLSKey: key}
			putJSON(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), map[string]any{"gateway": settings})
			loaded := loadOK(t, home)
			for _, path := range []string{loaded.Settings.Gateway.TLSCert, loaded.Settings.Gateway.TLSKey} {
				if !filepath.IsAbs(path) {
					t.Fatalf("TLS path did not resolve from the canonical root: %q", path)
				}
			}
			if got := GatewayBaseURL(loaded.Settings.Gateway); got != "https://[::1]:49876" {
				t.Fatalf("base URL = %q", got)
			}
			putJSON(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), loaded.Settings)
			if again := loadOK(t, home); !reflect.DeepEqual(again.Settings.Gateway, loaded.Settings.Gateway) {
				t.Fatal("gateway settings changed during JSON round-trip")
			}
		})
	}
}

func TestGatewayResourceURLsEncodeNames(t *testing.T) {
	settings := GatewaySettings{RouteKey: []byte("01234567890123456789012345678901")}
	for _, test := range []struct {
		name, harness, want string
	}{
		{"team:docs", "cursor", "http://127.0.0.1:43877/mcp/HO8PyWPGKLZ820po5JC15XOAZwbhFwmAUsxRQEtQttw/dGVhbTpkb2Nz/Y3Vyc29y"},
		{"café", "", "http://127.0.0.1:43877/mcp/bA4KYNTUuxZOMpNrNl60JOyjhSkdmFGVZ9HI7BUtvQ4/Y2Fmw6k/-"},
		{"/", "-", "http://127.0.0.1:43877/mcp/IVKvkAAY2Wu8WohFaWV3s4Q7PWqC8jVGOhyjID758AM/Lw/LQ"},
	} {
		if got := GatewayResourceURL(settings, test.name, test.harness); got != test.want {
			t.Errorf("resource URL for %q/%q = %q, want %q", test.name, test.harness, got, test.want)
		}
	}
}

func TestGatewayResourceURLsRequirePrivateKeyAndBindBothNames(t *testing.T) {
	for _, length := range []int{0, 1, 31, 33, 64} {
		if got := GatewayResourceURL(GatewaySettings{RouteKey: make([]byte, length)}, "server", "cursor"); got != "" {
			t.Fatalf("key length %d produced a fallback endpoint", length)
		}
	}
	settings := GatewaySettings{RouteKey: bytes.Repeat([]byte{0x31}, 32)}
	seen := map[string]bool{}
	for _, names := range [][2]string{{"ab", "c"}, {"a", "bc"}, {"ab", ""}, {"ab", "-"}, {"a", "c"}} {
		parts := strings.Split(GatewayResourceURL(settings, names[0], names[1]), "/")
		capability := parts[4]
		if seen[capability] {
			t.Fatal("different route name pairs reused an endpoint capability")
		}
		seen[capability] = true
	}
	first := GatewayResourceURL(settings, "server", "cursor")
	settings.RouteKey[0] ^= 1
	if first == GatewayResourceURL(settings, "server", "cursor") {
		t.Fatal("a different secret retained the endpoint capability")
	}
	encoded, err := json.Marshal(settings)
	if err != nil || bytes.Contains(encoded, []byte("routeKey")) ||
		bytes.Contains(encoded, []byte(base64.StdEncoding.EncodeToString(settings.RouteKey))) {
		t.Fatal("runtime routing key leaked into canonical settings JSON")
	}
}

func TestGatewaySettingsLeaveCanonicalUpstreamUnchanged(t *testing.T) {
	home := t.TempDir()
	canonical := `{"mcpServers":{"remote":{"url":"https://example.test/mcp?key=fixture-secret","headers":{"Authorization":"Bearer fixture-secret"}},"local":{"command":"fixture-command"}}}`
	path := filepath.Join(home, ".agentcfg", "mcp.json")
	putFile(t, path, canonical)
	before := loadOK(t, home)
	putFile(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), `{"gateway":{"enabled":true},"oauth":{"autoOpenBrowser":true}}`)
	after := loadOK(t, home)
	if !reflect.DeepEqual(before.Servers, after.Servers) {
		t.Fatal("gateway settings changed the canonical upstream or OAuth identity inputs")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != canonical {
		t.Fatal("canonical MCP file was changed")
	}
	encoded, err := json.Marshal(after.Settings.Gateway)
	if err != nil || strings.Contains(string(encoded), "fixture-secret") {
		t.Fatal("upstream credentials leaked into gateway settings")
	}
}
