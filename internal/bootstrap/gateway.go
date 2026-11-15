package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/fsstate"
	"agentcfg/internal/pathutil"
)

type gatewayProxy struct {
	home, digest string
	settings     config.GatewaySettings
}

// Gateway URLs alone are not proof of ownership: an ordinary local MCP can use
// the same address or path shape. Require the user-root ownership record, the
// exact current gateway origin/route and the unchanged managed native entry.
func managedGatewayProxies(home string) map[string]gatewayProxy {
	data, err := fsstate.ReadFile(filepath.Join(home, ".agentcfg", "sync-user.json"), "", 16<<20)
	if err != nil {
		return nil
	}
	var ownership struct {
		Version int                          `json:"version"`
		Configs map[string]map[string]string `json:"configs"`
	}
	if json.Unmarshal(data, &ownership) != nil || ownership.Version != 1 {
		return nil
	}
	cfg, err := config.Load(home)
	if err != nil {
		return nil
	}
	proxies := map[string]gatewayProxy{}
	for _, target := range targets {
		for _, relative := range target.configs {
			logical := filepath.Join(cfg.Root, filepath.FromSlash(relative))
			owned := ownership.Configs[logical]
			if len(owned) == 0 {
				continue
			}
			physical, err := pathutil.Resolve(logical)
			if err != nil {
				continue
			}
			for name, digest := range owned {
				// Canonical removal precedes the next sync. The unchanged native
				// entry and its signed route still prove ownership in that interval.
				key := gatewayProxyKey(target.name, physical, name)
				proxies[key] = gatewayProxy{home: cfg.Root, settings: cfg.Settings.Gateway, digest: digest}
			}
		}
	}
	return proxies
}

func gatewayProxyKey(harness, source, name string) string {
	return harness + "\x00" + source + "\x00" + name
}

func (p *Plan) isManagedGatewayProxy(harness, source, name string, raw json.RawMessage, server config.Server) (bool, error) {
	proxy, exists := p.gatewayProxies[gatewayProxyKey(harness, source, name)]
	if !exists || server.Type != "streamable-http" || !gatewayRouteCandidate(proxy.settings, name, harness, server.URL) {
		return false, nil
	}
	var entry any
	if json.Unmarshal(raw, &entry) != nil {
		return false, nil
	}
	encoded, err := json.Marshal(entry)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(encoded)) != proxy.digest {
		return false, nil
	}
	// Ordinary native entries never open a credential store. Only an intact,
	// owned capability route can require this read-only verification.
	proxy.settings.RouteKey, err = auth.GatewayKey(context.Background(), proxy.home, nil, false)
	if err != nil {
		return false, errors.New("an owned agentcfg HTTP gateway route cannot be verified; initialize the gateway before importing")
	}
	if config.GatewayResourceURL(proxy.settings, name, harness) != server.URL {
		return false, errors.New("an owned agentcfg HTTP gateway route no longer matches its routing key; synchronize it before importing")
	}
	return true, nil
}

func gatewayRouteCandidate(settings config.GatewaySettings, name, harness, rawURL string) bool {
	prefix := config.GatewayBaseURL(settings) + "/mcp/"
	if !strings.HasPrefix(rawURL, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(rawURL, prefix), "/")
	if len(parts) != 3 || parts[1] != base64.RawURLEncoding.EncodeToString([]byte(name)) {
		return false
	}
	expectedHarness := "-"
	if harness != "" {
		expectedHarness = base64.RawURLEncoding.EncodeToString([]byte(harness))
	}
	capability, err := base64.RawURLEncoding.DecodeString(parts[0])
	return err == nil && len(capability) == 32 && base64.RawURLEncoding.EncodeToString(capability) == parts[0] && parts[2] == expectedHarness
}
