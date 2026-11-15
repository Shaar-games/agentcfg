package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
)

var oauthBrowserExecutable = flag.String("agentcfg-browser-executable", "", "absolute Chromium executable for the optional isolated browser OAuth regression")
var oauthBrowserNode = flag.String("agentcfg-browser-node", "", "absolute Node executable with built-in WebSocket for the browser OAuth regression")

// This optional integration test is enabled explicitly; ordinary go test never
// discovers or launches an installed browser, nor reads its user's profile.
func TestNativeOAuthBrowserNavigation(t *testing.T) {
	if *oauthBrowserExecutable == "" || *oauthBrowserNode == "" {
		t.Skip("real-browser regression requires -agentcfg-browser-executable and -agentcfg-browser-node absolute test flags")
	}
	for _, executable := range []string{*oauthBrowserExecutable, *oauthBrowserNode} {
		info, err := os.Stat(executable)
		if !filepath.IsAbs(executable) || err != nil || !info.Mode().IsRegular() {
			t.Fatal("browser regression requires existing absolute binary paths")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	provider := newNativeProvider(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agentcfg"), 0700); err != nil {
		t.Fatal(err)
	}
	front := httptest.NewUnstartedServer(nil)
	address := front.Listener.Addr().String()
	base := "http://" + address
	settings := config.Settings{Harnesses: []string{"cursor"}, Gateway: config.GatewaySettings{Enabled: true, Listen: address}}
	entry := config.Server{Type: "streamable-http", URL: provider.server.URL + "/mcp", OAuth: &config.OAuthConfig{DynamicRegistration: true, RedirectURL: unusedNativeCallback(t)}}
	for name, value := range map[string]any{"agentcfg.json": settings, "mcp.json": map[string]any{"mcpServers": map[string]any{"fixture": entry}}} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".agentcfg", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := auth.NewEncryptedFileStore(root, bytes.Repeat([]byte{47}, 32))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(ctx, root, Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	var consentOrigin atomic.Value
	front.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/oauth/authorize" {
			classification := "other"
			switch r.Header.Get("Origin") {
			case base:
				classification = "expected"
			case "null":
				classification = "null"
			case "":
				classification = "absent"
			}
			consentOrigin.Store(classification)
		}
		handler.ServeHTTP(w, r)
	})
	front.Start()
	t.Cleanup(front.Close)
	settings.Gateway.RouteKey, err = auth.GatewayKey(ctx, root, store, false)
	if err != nil {
		t.Fatal(err)
	}
	resource := config.GatewayResourceURL(settings.Gateway, "fixture", "cursor")
	callbackURLs := make(chan string, 2)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		select {
		case callbackURLs <- "http://" + r.Host + r.URL.RequestURI():
		default:
			t.Error("duplicate browser callback")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>OAuth callback complete</title><p>Client callback received.</p>")
	}))
	t.Cleanup(callback.Close)
	redirect := callback.URL + "/callback"
	for _, name := range []string{"new_login", "cached_upstream_credentials"} {
		if !t.Run(name, func(t *testing.T) {
			registration, _ := json.Marshal(map[string]any{"client_name": "Chromium fixture", "redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none",
				"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/oauth/register", bytes.NewReader(registration))
			request.Header.Set("Content-Type", "application/json")
			response, err := front.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			var client struct {
				ClientID string `json:"client_id"`
			}
			err = json.NewDecoder(response.Body).Decode(&client)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusCreated || client.ClientID == "" {
				t.Fatal("fixture client registration failed")
			}
			verifier, state := rand.Text(), rand.Text()
			// OAuth PKCE verifiers must contain at least 43 characters.
			verifier += rand.Text()
			sum := sha256.Sum256([]byte(verifier))
			query := url.Values{"client_id": {client.ClientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "resource": {resource}, "scope": {"mcp:access"},
				"state": {state}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
			driver, err := filepath.Abs(filepath.Join("..", "..", "test", "integration", "browser-oauth.mjs"))
			if err != nil {
				t.Fatal(err)
			}
			browserRoot := t.TempDir()
			profile := filepath.Join(browserRoot, "profile")
			if err := os.Mkdir(profile, 0700); err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(map[string]any{"executable": *oauthBrowserExecutable, "profile": profile, "url": base + "/oauth/authorize?" + query.Encode(), "callbackURL": redirect, "providerURL": provider.server.URL, "timeoutMs": 20000})
			command := exec.CommandContext(ctx, *oauthBrowserNode, driver)
			command.Env = browserFixtureEnvironment(t, browserRoot)
			command.Dir = browserRoot
			command.Stdin = bytes.NewReader(input)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("isolated Chromium OAuth navigation failed: %v; consent Origin=%v; provider logins=%d exchanges=%d; %s", err, consentOrigin.Load(), provider.logins.Load(), provider.exchanges.Load(), output)
			}
			var result struct {
				Completed, Submitted bool
				CSPViolations        []string
				Browser              string
			}
			if json.Unmarshal(output, &result) != nil || !result.Completed || !result.Submitted || len(result.CSPViolations) != 0 {
				t.Fatalf("invalid browser navigation result: %s", output)
			}
			if consentOrigin.Load() != "expected" {
				t.Fatal("browser form did not preserve the required same-origin Origin header")
			}
			var callbackURL string
			select {
			case callbackURL = <-callbackURLs:
			case <-ctx.Done():
				t.Fatal("browser did not reach the native client callback")
			}
			grant, err := auth.ParseAuthorizationResponse(redirect, callbackURL)
			if err != nil || grant.State != state || grant.Code == "" || strings.Contains(grant.Code, "upstream-") {
				t.Fatal("browser callback did not contain a separate bound client code")
			}
			form := url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ClientID}, "code": {grant.Code}, "code_verifier": {verifier}, "redirect_uri": {redirect}, "resource": {resource}}
			request, _ = http.NewRequestWithContext(ctx, http.MethodPost, base+"/oauth/token", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response, err = front.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			var token struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
			}
			err = json.NewDecoder(response.Body).Decode(&token)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK || token.AccessToken == "" || token.RefreshToken == "" || strings.Contains(token.AccessToken, "upstream-") {
				t.Fatal("native client code did not exchange for its own credentials")
			}
			if provider.logins.Load() != 1 || provider.exchanges.Load() != 1 {
				t.Fatalf("upstream consent was repeated: logins=%d exchanges=%d", provider.logins.Load(), provider.exchanges.Load())
			}
			t.Logf("%s: submitted actual consent form, reached separate client callback, exchanged PKCE code; CSP violations=0, upstream logins=1", result.Browser)
		}) {
			return
		}
	}
}

func browserFixtureEnvironment(t *testing.T, root string) []string {
	t.Helper()
	home := filepath.Join(root, "home")
	paths := map[string]string{
		"HOME": home, "USERPROFILE": home,
		"APPDATA": filepath.Join(home, "AppData", "Roaming"), "LOCALAPPDATA": filepath.Join(home, "AppData", "Local"),
		"TMP": filepath.Join(root, "tmp"), "TEMP": filepath.Join(root, "tmp"), "TMPDIR": filepath.Join(root, "tmp"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"), "XDG_CACHE_HOME": filepath.Join(root, "cache"),
		"XDG_DATA_HOME": filepath.Join(root, "data"), "XDG_STATE_HOME": filepath.Join(root, "state"), "XDG_RUNTIME_DIR": filepath.Join(root, "runtime"),
	}
	environment := []string{"LANG=C", "TZ=UTC"}
	for name, path := range paths {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		environment = append(environment, name+"="+path)
	}
	if volume := filepath.VolumeName(home); volume != "" {
		environment = append(environment, "HOMEDRIVE="+volume, "HOMEPATH="+strings.TrimPrefix(home, volume))
	}
	// Preserve only OS locations needed to launch executables. No user browser,
	// Node, proxy, keyring, profile, or startup configuration is inherited.
	for _, name := range []string{"SystemRoot", "WINDIR", "COMSPEC", "SYSTEMDRIVE"} {
		if value := os.Getenv(name); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	search := []string{filepath.Dir(*oauthBrowserNode), filepath.Dir(*oauthBrowserExecutable)}
	if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
		search = append(search, filepath.Join(systemRoot, "System32"))
	}
	return append(environment, "PATH="+strings.Join(search, string(os.PathListSeparator)), "PATHEXT=.COM;.EXE;.BAT;.CMD")
}
