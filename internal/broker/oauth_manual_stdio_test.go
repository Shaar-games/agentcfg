package broker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestOAuthStdioManualLoginWithoutAuthorizationCapability(t *testing.T) {
	for _, sharing := range []string{"shared", "isolated"} {
		for _, modern := range []bool{false, true} {
			t.Run(sharing+map[bool]string{false: "-legacy", true: "-modern"}[modern], func(t *testing.T) {
				fixture := newOAuthStdioFixture(t)
				root := oauthStdioHome(t, fixture.http.URL+"/mcp", sharing, nil)
				serveOAuthStdioBroker(t, root)
				peer := connectOAuthStdio(t, root, nil)
				begin := func(p *oauthStdioClient, id string) {
					if modern {
						if err := p.writer.Write(modernMessage(id, "server/discover", map[string]any{})); err != nil {
							t.Fatal(err)
						}
					} else {
						p.send(t, "initialize", id, map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"elicitation": map[string]any{"url": map[string]any{}}}, "clientInfo": map[string]string{"name": "manual-client", "version": "1"}})
					}
				}
				begin(peer, "before-login")
				failure := peer.read(t, "before-login", false)
				if !strings.Contains(string(failure["error"]), "agentcfg auth login fixture") {
					t.Fatal("stdio did not explain the manual login requirement")
				}
				if fixture.browserCalls.Load() != 0 || fixture.exchanges.Load() != 0 || fixture.registrations.Load() != 0 || fixture.metadataRequests.Load() != 0 {
					t.Fatal("noninteractive stdio started discovery, registration or consent")
				}
				loaded, err := config.Load(root)
				if err != nil {
					t.Fatal(err)
				}
				server := loaded.Servers["fixture"]
				server.Name = "fixture"
				server = auth.InteractiveServer(server)
				status, err := auth.Inspect(t.Context(), server, auth.Options{Home: root})
				if err != nil || !status.AuthorizationObserved || !status.LoginRequired {
					t.Fatalf("login requirement was not visible to other commands: %+v %v", status, err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if err := auth.Login(ctx, server, auth.Options{Home: root, AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
					return fixture.fetch(ctx, server.OAuth.RedirectURL, args)
				}}); err != nil {
					t.Fatal(err)
				}
				status, err = auth.Inspect(t.Context(), server, auth.Options{Home: root})
				if err != nil || status.LoginRequired || !status.Authenticated {
					t.Fatalf("completed manual login still requires authentication: %+v %v", status, err)
				}
				for index, p := range []*oauthStdioClient{peer, connectOAuthStdio(t, root, nil)} {
					id := map[int]string{0: "same-client", 1: "new-client"}[index]
					begin(p, id)
					p.read(t, id, true)
					if !modern {
						if err := p.writer.Write(wire.Notification("notifications/initialized", nil)); err != nil {
							t.Fatal(err)
						}
					}
					if modern {
						if err := p.writer.Write(modernMessage(id+"-tools", "tools/list", map[string]any{})); err != nil {
							t.Fatal(err)
						}
					} else {
						p.send(t, "tools/list", id+"-tools", map[string]any{})
					}
					catalog := p.read(t, id+"-tools", true)
					if !strings.Contains(string(catalog["result"]), `"echo"`) || strings.Contains(string(catalog["result"]), "agentcfg_authenticate") {
						t.Fatal("stdio did not retain the real upstream catalog")
					}
					assertOAuthStdioSecretsAbsent(t, p, fixture.http.URL)
				}
				if fixture.browserCalls.Load() != 1 || fixture.exchanges.Load() != 1 {
					t.Fatal("manual credentials triggered another consent")
				}
			})
		}
	}
}

func TestOAuthStdioManualToolStepUpSurvivesSeparateLogin(t *testing.T) {
	f := newOAuthStdioFixture(t)
	root := oauthStdioHome(t, f.http.URL+"/mcp", "shared", nil)
	loaded, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	server := auth.InteractiveServer(loaded.Servers["fixture"])
	beforeKey := auth.CredentialKey(server)
	login := func() {
		t.Helper()
		if err := auth.Login(t.Context(), server, auth.Options{Home: root, AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
			return f.fetch(ctx, server.OAuth.RedirectURL, args)
		}}); err != nil {
			t.Fatal(err)
		}
	}
	login()
	serveOAuthStdioBroker(t, root)
	peer := connectOAuthStdio(t, root, nil)
	peer.initialize(t)
	f.stepUp.Store(true)
	peer.send(t, "tools/call", "needs-write", map[string]any{"name": "echo", "arguments": map[string]any{}})
	failure := peer.read(t, "needs-write", false)
	if !strings.Contains(string(failure["error"]), "agentcfg auth login fixture") || f.toolCalls.Load() != 1 || f.toolEffects.Load() != 0 {
		t.Fatal("scope refusal executed or repeated a tool instead of requiring manual login")
	}
	status, err := auth.Inspect(t.Context(), server, auth.Options{Home: root})
	if err != nil || !status.LoginRequired || !slices.Contains(status.RequiredScopes, "tools.write") {
		t.Fatalf("tool scope requirement was not retained: %+v %v", status, err)
	}
	if f.browserCalls.Load() != 1 || f.exchanges.Load() != 1 || f.registrations.Load() != 1 {
		t.Fatal("manual tool failure silently initiated OAuth")
	}
	login()
	f.mu.Lock()
	registeredScopes := f.registeredScopes
	f.mu.Unlock()
	if !slices.Contains(strings.Fields(registeredScopes), "tools.write") {
		t.Fatal("manual DCR did not request the previously refused tool scope")
	}
	status, err = auth.Inspect(t.Context(), server, auth.Options{Home: root})
	if err != nil || status.LoginRequired || !slices.Contains(status.Scopes, "tools.write") {
		t.Fatalf("separate login did not resolve the tool's scope requirement: %+v %v", status, err)
	}
	if f.toolCalls.Load() != 1 || f.toolEffects.Load() != 0 {
		t.Fatal("login replayed an earlier failed tool")
	}
	peer.call(t, "user-retries-write", true)
	if f.toolCalls.Load() != 2 || f.toolEffects.Load() != 1 || f.browserCalls.Load() != 2 || f.exchanges.Load() != 2 {
		t.Fatal("explicit retry did not execute once with the new grant")
	}
	after, err := config.Load(root)
	if err != nil || auth.CredentialKey(auth.InteractiveServer(after.Servers["fixture"])) != beforeKey || after.Servers["fixture"].OAuth != nil {
		t.Fatal("manual step-up rewrote canonical configuration or credential identity")
	}
	assertOAuthStdioSecretsAbsent(t, peer, f.http.URL)
}

func TestOAuthStdioAutomaticPeerCanJoinManualBackend(t *testing.T) {
	f := newOAuthStdioFixture(t)
	root := oauthStdioHome(t, f.http.URL+"/mcp", "shared", nil)
	serveOAuthStdioBroker(t, root)
	manual := connectOAuthStdio(t, root, nil)
	manual.beginInitialize(t)
	manual.read(t, "init", false)
	if f.metadataRequests.Load() != 0 || f.registrations.Load() != 0 {
		t.Fatal("manual backend started OAuth discovery")
	}
	automatic := connectOAuthStdio(t, root, f.fetch)
	automatic.initialize(t)
	manual.initialize(t)
	if f.browserCalls.Load() != 1 || f.exchanges.Load() != 1 || f.registrations.Load() != 1 {
		t.Fatal("opted-in peer did not authorize the existing shared backend exactly once")
	}
	assertOAuthStdioSecretsAbsent(t, manual, f.http.URL)
	assertOAuthStdioSecretsAbsent(t, automatic, f.http.URL)
}

func TestOAuthStdioOrdinaryForbiddenDoesNotLoginOrRetry(t *testing.T) {
	f := newOAuthStdioFixture(t)
	f.public.Store(true)
	f.forbiddenTool.Store(true)
	root := oauthStdioHome(t, f.http.URL+"/mcp", "shared", nil)
	t.Setenv("AGENTCFG_OAUTH_KEY", "must-not-resolve-for-permission-denial")
	serveOAuthStdioBroker(t, root)
	peer := connectOAuthStdio(t, root, f.fetch)
	peer.initialize(t)
	peer.send(t, "tools/call", "forbidden", map[string]any{"name": "echo", "arguments": map[string]any{}})
	failure := peer.read(t, "forbidden", false)
	if !strings.Contains(string(failure["error"]), "access denied") || strings.Contains(string(failure["error"]), "auth login") {
		t.Fatalf("ordinary 403 became a login request: %s", failure["error"])
	}
	if f.toolCalls.Load() != 1 || f.toolEffects.Load() != 0 || f.browserCalls.Load() != 0 || f.metadataRequests.Load() != 0 || f.registrations.Load() != 0 {
		t.Fatal("ordinary permission denial repeated a tool or initiated OAuth")
	}
	if _, err := os.Stat(filepath.Join(root, ".agentcfg", "credentials")); !os.IsNotExist(err) {
		t.Fatalf("ordinary permission denial created credential state: %v", err)
	}
}

func TestOAuthStdioAutomaticScopeChallengeHasOneEffect(t *testing.T) {
	f := newOAuthStdioFixture(t)
	root := oauthStdioHome(t, f.http.URL+"/mcp", "shared", nil)
	serveOAuthStdioBroker(t, root)
	peer := connectOAuthStdio(t, root, f.fetch)
	peer.initialize(t)
	f.stepUp.Store(true)
	peer.call(t, "authorize-and-call", true)
	if f.toolCalls.Load() != 2 || f.toolEffects.Load() != 1 || f.browserCalls.Load() != 2 || f.exchanges.Load() != 2 {
		t.Fatal("SDK did not resume an explicitly denied scope challenge with exactly one effect")
	}
	assertOAuthStdioSecretsAbsent(t, peer, f.http.URL)
}
