package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/wire"
)

type cliOAuthFixture struct {
	server          *httptest.Server
	redirect        string
	callbackAddress string
	exchanges       *atomic.Int32
	authenticated   *atomic.Int32
}

func newCLIAuthFixture(t *testing.T) *cliOAuthFixture {
	t.Helper()
	redirect, callbackAddress := connectOAuthCallback(t)
	var fixture *httptest.Server
	var mu sync.Mutex
	var challenge string
	var exchanges, authenticated atomic.Int32
	fixture = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource":
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": fixture.URL + "/mcp", "authorization_servers": []string{fixture.URL}})
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": fixture.URL, "authorization_endpoint": fixture.URL + "/authorize", "token_endpoint": fixture.URL + "/token",
				"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code"},
				"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"},
				"authorization_response_iss_parameter_supported": true,
			})
		case "/authorize":
			q := r.URL.Query()
			if q.Get("redirect_uri") != redirect || q.Get("client_id") != "manual-fixture" || q.Get("state") == "" || q.Get("code_challenge_method") != "S256" {
				http.Error(w, "invalid local fixture authorization", http.StatusBadRequest)
				return
			}
			mu.Lock()
			challenge = q.Get("code_challenge")
			mu.Unlock()
			callback := redirect + "?" + url.Values{"code": {"fixture-code"}, "state": {q.Get("state")}, "iss": {fixture.URL}}.Encode()
			http.Redirect(w, r, callback, http.StatusFound)
		case "/token":
			if r.ParseForm() != nil {
				http.Error(w, "invalid local fixture form", http.StatusBadRequest)
				return
			}
			mu.Lock()
			want := challenge
			mu.Unlock()
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if want == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != want || r.Form.Get("code") != "fixture-code" ||
				r.Form.Get("resource") != fixture.URL+"/mcp" || r.Form.Get("redirect_uri") != redirect {
				http.Error(w, "invalid local fixture grant", http.StatusBadRequest)
				return
			}
			exchanges.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access", "token_type": "Bearer", "refresh_token": "fixture-refresh", "expires_in": 3600})
		case "/mcp":
			if r.Header.Get("Authorization") != "Bearer fixture-access" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf("Bearer resource_metadata=%q", fixture.URL+"/.well-known/oauth-protected-resource/mcp"))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			authenticated.Add(1)
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			var message wire.Message
			if json.NewDecoder(r.Body).Decode(&message) != nil {
				http.Error(w, "invalid local MCP request", http.StatusBadRequest)
				return
			}
			if !message.HasID() {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			if message.Method() == "server/discover" {
				_ = json.NewEncoder(w).Encode(wire.Result(message.ID(), map[string]any{"supportedVersions": []string{wire.ModernProtocolVersion}, "capabilities": map[string]any{"tools": map[string]any{}}}))
				return
			}
			if message.Method() == "tools/list" {
				_ = json.NewEncoder(w).Encode(wire.Result(message.ID(), map[string]any{"tools": []any{map[string]any{
					"name": "fixture-echo", "inputSchema": map[string]any{"type": "object"},
				}}}))
				return
			}
			if message.Method() != "initialize" {
				_ = json.NewEncoder(w).Encode(wire.Error(message.ID(), -32601, "unknown local fixture method"))
				return
			}
			var params struct{ ProtocolVersion string }
			_ = json.Unmarshal(message["params"], &params)
			_ = json.NewEncoder(w).Encode(wire.Result(message.ID(), map[string]any{
				"protocolVersion": params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "manual-login-fixture", "version": "1"},
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.Close)
	return &cliOAuthFixture{server: fixture, redirect: redirect, callbackAddress: callbackAddress, exchanges: &exchanges, authenticated: &authenticated}
}

// Explicit terminal login must remain usable when automatic browser opening is
// disabled. The output reader simulates following the printed URL; no browser,
// account or credential outside this test's temporary directory is used.
func TestAuthLoginManualWorksWithAutomaticBrowserDisabled(t *testing.T) {
	t.Setenv("AGENTCFG_OAUTH_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	testHome := t.TempDir()
	local := newCLIAuthFixture(t)
	fixture, redirect, callbackAddress := local.server, local.redirect, local.callbackAddress
	exchanges, authenticated := local.exchanges, local.authenticated
	canonical := filepath.Join(testHome, ".agentcfg")
	if err := os.MkdirAll(canonical, 0700); err != nil {
		t.Fatal(err)
	}
	settings := []byte(`{"oauth":{"autoOpenBrowser":false}}`)
	definition, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{
		"url": fixture.URL + "/mcp", "oauth": map[string]any{"clientId": "manual-fixture", "issuer": fixture.URL, "redirectUrl": redirect},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"agentcfg.json": settings, "mcp.json": definition} {
		if err := os.WriteFile(filepath.Join(canonical, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	output := &manualOAuthURLConsumer{client: client, authorizePrefix: fixture.URL + "/authorize?"}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := runAuth(ctx, testHome, []string{"login", "fixture", "--no-browser"}, strings.NewReader(""), output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !output.followed || exchanges.Load() != 1 || authenticated.Load() == 0 || !strings.Contains(output.String(), "OAuth login completed for fixture") {
		t.Fatalf("explicit manual command did not finish: followed=%t exchanges=%d authenticated=%d", output.followed, exchanges.Load(), authenticated.Load())
	}
	cfg, err := config.Load(testHome)
	if err != nil {
		t.Fatal(err)
	}
	state, err := auth.Inspect(ctx, cfg.Servers["fixture"], auth.Options{Home: testHome})
	if err != nil || !state.Authenticated || !state.Stored {
		t.Fatalf("manual command did not persist usable credentials: %+v %v", state, err)
	}
	for name, before := range map[string][]byte{"agentcfg.json": settings, "mcp.json": definition} {
		after, err := os.ReadFile(filepath.Join(canonical, name))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("manual login modified %s: %v", name, err)
		}
	}
	assertConnectOAuthCallbackClosed(t, callbackAddress)
}

type manualOAuthURLConsumer struct {
	bytes.Buffer
	client          *http.Client
	authorizePrefix string
	followed        bool
}

func (w *manualOAuthURLConsumer) Write(p []byte) (int, error) {
	n, _ := w.Buffer.Write(p)
	if !w.followed {
		for _, line := range strings.Split(w.String(), "\n") {
			if !strings.HasPrefix(line, w.authorizePrefix) {
				continue
			}
			w.followed = true
			response, err := w.client.Get(line)
			if err != nil {
				return n, err
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				return n, fmt.Errorf("manual fixture callback returned HTTP %d", response.StatusCode)
			}
		}
	}
	return n, nil
}
