package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestOAuthBrowserSettingsLoad(t *testing.T) {
	for _, test := range []struct {
		name    string
		data    string
		enabled bool
	}{
		{name: "missing settings"},
		{name: "omitted oauth", data: `{}`},
		{name: "empty oauth", data: `{"oauth":{}}`},
		{name: "explicit false", data: `{"oauth":{"autoOpenBrowser":false}}`},
		{name: "explicit true", data: `{"oauth":{"autoOpenBrowser":true}}`, enabled: true},
		{name: "whitespace", data: "{\"oauth\": {\"autoOpenBrowser\": \n true \t }}", enabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if test.data != "" {
				putFile(t, filepath.Join(root, ".agentcfg", "agentcfg.json"), test.data)
			}
			cfg := loadOK(t, root)
			if got := cfg.Settings.OAuth.AutoOpenBrowser; got != test.enabled {
				t.Fatalf("autoOpenBrowser = %v, want %v", got, test.enabled)
			}
			if len(cfg.Warnings) != 0 {
				t.Fatalf("valid OAuth settings produced warnings: %v", cfg.Warnings)
			}
		})
	}
}

func TestOAuthBrowserSettingsRejectInvalidValues(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
		want string
	}{
		{"null object", `{"oauth":null}`, "oauth must be an object"},
		{"array object", `{"oauth":[]}`, "oauth must be an object"},
		{"boolean object", `{"oauth":true}`, "oauth must be an object"},
		{"number object", `{"oauth":1}`, "oauth must be an object"},
		{"string object", `{"oauth":"true"}`, "oauth must be an object"},
		{"null boolean", `{"oauth":{"autoOpenBrowser":null}}`, "oauth.autoOpenBrowser must be a boolean"},
		{"whitespace null", "{\"oauth\":{\"autoOpenBrowser\": \n null \t }}", "oauth.autoOpenBrowser must be a boolean"},
		{"string boolean", `{"oauth":{"autoOpenBrowser":"true"}}`, "oauth.autoOpenBrowser must be a boolean"},
		{"number boolean", `{"oauth":{"autoOpenBrowser":1}}`, "oauth.autoOpenBrowser must be a boolean"},
		{"array boolean", `{"oauth":{"autoOpenBrowser":[]}}`, "oauth.autoOpenBrowser must be a boolean"},
		{"object boolean", `{"oauth":{"autoOpenBrowser":{}}}`, "oauth.autoOpenBrowser must be a boolean"},
		{"unknown field", `{"oauth":{"openBrowser":true}}`, `oauth: unknown field "openBrowser"`},
		{"wrong case", `{"oauth":{"AutoOpenBrowser":true}}`, `oauth: unknown field "AutoOpenBrowser"`},
		{"wrong scope", `{"autoOpenBrowser":true}`, `unknown field "autoOpenBrowser"`},
		{"duplicate boolean", `{"oauth":{"autoOpenBrowser":false,"autoOpenBrowser":true}}`, `duplicate JSON key "autoOpenBrowser"`},
		{"duplicate oauth", `{"oauth":{},"oauth":{"autoOpenBrowser":true}}`, `duplicate JSON key "oauth"`},
		{"malformed JSON", `{"oauth":{"autoOpenBrowser":true},}`, "invalid character"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			putFile(t, filepath.Join(root, ".agentcfg", "agentcfg.json"), test.data)
			_, err := Load(root)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestOAuthBrowserSettingsJSONRoundTrip(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		root := t.TempDir()
		settings := defaultSettings()
		settings.OAuth.AutoOpenBrowser = enabled
		putJSON(t, filepath.Join(root, ".agentcfg", "agentcfg.json"), settings)
		if got := loadOK(t, root).Settings.OAuth.AutoOpenBrowser; got != enabled {
			t.Fatalf("round-trip autoOpenBrowser = %v, want %v", got, enabled)
		}
	}
}

func TestOAuthBrowserSettingsSeparateFromServerOAuth(t *testing.T) {
	root := t.TempDir()
	canonicalServers(t, root, testObject{"remote": testObject{"url": "https://example.test/mcp"}})
	putFile(t, filepath.Join(root, ".agentcfg", "agentcfg.json"), `{
		"oauth":{"autoOpenBrowser":true},
		"serverOverrides":{"remote":{"oauth":{"clientId":"fixture-client","redirectUrl":"http://127.0.0.1:8912/callback"}}}
	}`)
	cfg := loadOK(t, root)
	if !cfg.Settings.OAuth.AutoOpenBrowser {
		t.Fatal("global browser preference was lost")
	}
	server := cfg.Servers["remote"]
	if server.OAuth == nil || server.OAuth.ClientID != "fixture-client" || server.OAuth.RedirectURL != "http://127.0.0.1:8912/callback" {
		t.Fatalf("per-server OAuth configuration was lost: %+v; warnings: %v", server.OAuth, cfg.Warnings)
	}
}
