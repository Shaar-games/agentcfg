package broker

import (
	"context"
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
				if !strings.Contains(string(failure["error"]), "agentcfg auth login") {
					t.Fatal("stdio did not explain the manual login requirement")
				}
				if fixture.browserCalls.Load() != 0 || fixture.exchanges.Load() != 0 {
					t.Fatal("noninteractive stdio started an OAuth login")
				}
				loaded, err := config.Load(root)
				if err != nil {
					t.Fatal(err)
				}
				server := loaded.Servers["fixture"]
				server.Name = "fixture"
				server = auth.InteractiveServer(server)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if err := auth.Login(ctx, server, auth.Options{Home: root, AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
					return fixture.fetch(ctx, server.OAuth.RedirectURL, args)
				}}); err != nil {
					t.Fatal(err)
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
