package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"agentcfg/internal/auth"
)

func TestAuthenticationNoticesAcrossReadOnlyCommands(t *testing.T) {
	t.Setenv("AGENTCFG_OAUTH_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	home := t.TempDir()
	for name, value := range map[string]string{
		"HOME": home, "USERPROFILE": home,
		"APPDATA": filepath.Join(home, "roaming"), "LOCALAPPDATA": filepath.Join(home, "cache"),
		"XDG_CONFIG_HOME": filepath.Join(home, "config"), "XDG_CACHE_HOME": filepath.Join(home, "cache"),
	} {
		t.Setenv(name, value)
	}
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "inspection must not connect", http.StatusUnauthorized)
	}))
	defer remote.Close()
	definition, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"login-needed":   map[string]any{"url": remote.URL + "/protected", "oauth": map[string]any{"clientId": "fixture", "redirectUrl": "http://127.0.0.1:45781/callback"}},
		"public-unknown": map[string]any{"url": remote.URL + "/public"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	writeScopeFixture(t, filepath.Join(home, ".agentcfg", "mcp.json"), string(definition))
	writeScopeFixture(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), `{"harnesses":["cursor"],"oauth":{"autoOpenBrowser":true}}`)
	before := projectSnapshot(t, home)
	for _, args := range [][]string{
		{"list", "--plain"}, {"mcps", "--plain"}, {"validate", "--plain"}, {"sync", "--plain"},
		{"status", "--json"}, {"auth", "status", "login-needed", "--json"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			if err := run(t.Context(), args, strings.NewReader(""), &output, &diagnostics); err != nil {
				t.Fatalf("inspection failed: %v; stderr=%s", err, diagnostics.String())
			}
			if !strings.Contains(output.String(), "agentcfg auth login login-needed") {
				t.Fatalf("login action is missing: %s", output.String())
			}
			if strings.Contains(output.String(), "agentcfg auth login public-unknown") {
				t.Fatal("unobserved public server was incorrectly marked login required")
			}
			if args[0] == "status" {
				var status struct {
					Running        bool                   `json:"running"`
					Authentication []serverAuthentication `json:"authentication"`
				}
				if err := json.Unmarshal(output.Bytes(), &status); err != nil {
					t.Fatalf("status is not clean JSON: %v", err)
				}
				if status.Running || len(status.Authentication) != 2 || status.Authentication[0].State != auth.SummaryLoginRequired || status.Authentication[1].State != auth.SummaryUnknown {
					t.Fatalf("unexpected local authentication status: %+v", status)
				}
			}
			if args[0] == "auth" {
				var status struct {
					auth.Status
					Authentication auth.Summary `json:"authentication"`
				}
				if err := json.Unmarshal(output.Bytes(), &status); err != nil || !status.LoginRequired || status.Authentication.State != auth.SummaryLoginRequired {
					t.Fatalf("auth status disagrees with its login action: %+v, %v", status, err)
				}
			}
			if requests.Load() != 0 {
				t.Fatal("read-only inspection contacted a remote MCP")
			}
			if !sameProjectSnapshot(before, projectSnapshot(t, home)) {
				t.Fatal("read-only inspection changed the temporary profile")
			}
		})
	}
}
