package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/clientauth"
	"agentcfg/internal/config"
	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

type upstreamChallenge struct {
	status        int
	header        http.Header
	method        string
	authorization string
	expires       time.Time
}

func (p *Proxy) authOptions(fetch sdkauth.AuthorizationCodeFetcher) auth.Options {
	return auth.Options{Home: p.opts.Home, Store: p.opts.Store, HTTPClient: p.opts.HTTPClient, AuthorizationCodeFetcher: fetch}
}

func (p *Proxy) upstreamToken(ctx context.Context, server config.Server) (string, error) {
	effective := auth.InteractiveServer(server)
	if effective.OAuth == nil {
		return "", nil
	}
	// NewHandler reloads the encrypted record for each request and can refresh it.
	// It has no fetcher here, so normal MCP traffic never initiates browser consent.
	handler, err := auth.NewHandler(ctx, effective, p.authOptions(nil))
	if err != nil {
		if server.OAuth == nil && errors.Is(err, auth.ErrSecureStoreUnavailable) {
			return "", nil
		}
		return "", err
	}
	source, err := handler.TokenSource(ctx)
	if err != nil {
		return "", err
	}
	if source == nil {
		return "", nil
	}
	token, err := source.Token()
	if err != nil {
		// URL-only public resources remain usable without an upstream grant.
		if server.OAuth == nil && proxyNeedsConsent(err) {
			return "", nil
		}
		return "", err
	}
	if token == nil || token.AccessToken == "" {
		return "", auth.ErrLoginRequired
	}
	return token.AccessToken, nil
}

func proxyNeedsConsent(err error) bool {
	if errors.Is(err, auth.ErrLoginRequired) {
		return true
	}
	var failure *oauth2.RetrieveError
	return errors.As(err, &failure) && failure.ErrorCode == "invalid_grant"
}

func proxyAuthorizationChallenge(response *http.Response) (bool, bool) {
	if response.StatusCode == http.StatusUnauthorized {
		return true, false
	}
	if response.StatusCode != http.StatusForbidden {
		return false, false
	}
	challenges, err := oauthex.ParseWWWAuthenticate(response.Header.Values("WWW-Authenticate"))
	if err != nil {
		return false, false
	}
	for _, challenge := range challenges {
		if strings.EqualFold(challenge.Scheme, "bearer") && challenge.Params["error"] == "insufficient_scope" {
			return true, true
		}
	}
	return false, false
}

func proxyChallengeKey(resource clientauth.Resource) string {
	// Upstream credentials belong to the canonical resource. A native client may
	// register a new downstream ClientID while responding to this same challenge.
	// Local consent still precedes Authorize, and session grants remain separate.
	sum := sha256.Sum256([]byte(resource.Identity))
	return hex.EncodeToString(sum[:])
}

func (p *Proxy) rememberChallenge(resource clientauth.Resource, request *http.Request, response *http.Response) bool {
	values := response.Header.Values("WWW-Authenticate")
	size := 0
	for _, value := range values {
		size += len(value)
	}
	if size > 16384 || len(values) > 16 {
		return false
	}
	key := proxyChallengeKey(resource)
	p.mu.Lock()
	defer p.mu.Unlock()
	for current, challenge := range p.challenges {
		if time.Now().After(challenge.expires) {
			delete(p.challenges, current)
		}
	}
	if _, exists := p.challenges[key]; !exists && len(p.challenges) >= proxyChallengeLimit {
		return false
	}
	p.challenges[key] = upstreamChallenge{status: response.StatusCode, method: request.Method,
		header: http.Header{"Www-Authenticate": append([]string(nil), values...)}, authorization: request.Header.Get("Authorization"), expires: time.Now().Add(proxyChallengeTTL)}
	return true
}

func (p *Proxy) savedChallenge(resource clientauth.Resource) (upstreamChallenge, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := proxyChallengeKey(resource)
	challenge, ok := p.challenges[key]
	if !ok || time.Now().After(challenge.expires) {
		delete(p.challenges, key)
		return upstreamChallenge{}, false
	}
	challenge.header = challenge.header.Clone()
	return challenge, true
}

// Authorize completes upstream consent while the local authority owns its own
// client grant, PKCE challenge and callback. No upstream token is returned.
func (p *Proxy) Authorize(parent context.Context, resource clientauth.Resource, principal clientauth.Principal, present func(string) error, completeURL string) error {
	if p.opts.Resolve == nil || resource.Identity == "" || principal.ClientID == "" {
		return errors.New("MCP authorization resource is unavailable")
	}
	if err := proxyCompletionURL(resource.URL, completeURL); err != nil {
		return err
	}
	server, err := p.opts.Resolve(resource)
	if err != nil {
		return errors.New("MCP authorization resource is unavailable")
	}
	target, headers, err := proxyServer(server)
	if err != nil {
		return errors.New("Invalid upstream MCP configuration")
	}
	ctx, releaseContext := p.requestContext(parent)
	defer releaseContext()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	effective := auth.InteractiveServer(server)
	gateKey := resource.Identity
	if effective.OAuth != nil {
		redirect, _ := url.Parse(effective.OAuth.RedirectURL)
		if redirect != nil {
			gateKey = strings.ToLower(redirect.Host)
		}
	}
	release, err := p.lockAuthorization(ctx, gateKey)
	if err != nil {
		return err
	}
	defer release()
	challenge, challenged := p.savedChallenge(resource)
	token, err := p.upstreamToken(ctx, server)
	if err != nil && !proxyNeedsConsent(err) {
		return errors.New("MCP upstream credentials could not be loaded or refreshed")
	}
	if !challenged && token != "" {
		return nil
	}
	if !challenged {
		request, response, err := p.probeUpstream(ctx, target, headers, token)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		needed, _ := proxyAuthorizationChallenge(response)
		if !needed {
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil
			}
			return errors.New("MCP upstream did not accept the authorization probe")
		}
		challenge = upstreamChallenge{status: response.StatusCode, header: response.Header.Clone(), method: request.Method,
			authorization: request.Header.Get("Authorization")}
	}
	if effective.OAuth == nil {
		return errors.New("MCP upstream requires its configured authentication credentials")
	}
	fetcher := func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		return proxyFetchAuthorization(ctx, effective.OAuth.RedirectURL, args, present, completeURL)
	}
	handler, err := auth.NewInteractiveHandler(ctx, effective, p.authOptions(fetcher))
	if err != nil {
		return errors.New("MCP upstream OAuth could not be initialized")
	}
	request := (&http.Request{Method: challenge.method, URL: target, Header: headers.Clone()}).WithContext(ctx)
	request.Header.Set("Authorization", challenge.authorization)
	response := &http.Response{StatusCode: challenge.status, Header: challenge.header, Body: io.NopCloser(strings.NewReader(""))}
	if err := handler.Authorize(ctx, request, response); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		return errors.New("MCP upstream OAuth authorization did not complete")
	}
	// Authorize returns only after the SDK has validated and persisted its grant.
	source, err := handler.TokenSource(ctx)
	if err != nil || source == nil {
		return errors.New("MCP upstream OAuth returned no stored credentials")
	}
	stored, err := source.Token()
	if err != nil || stored == nil || stored.AccessToken == "" {
		return errors.New("MCP upstream OAuth credentials could not be used")
	}
	p.mu.Lock()
	delete(p.challenges, proxyChallengeKey(resource))
	p.mu.Unlock()
	return nil
}

func proxyCompletionURL(resource, complete string) error {
	u, err := url.Parse(complete)
	base, baseErr := url.Parse(resource)
	if err != nil || baseErr != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Scheme != base.Scheme || !strings.EqualFold(u.Host, base.Host) {
		return errors.New("Invalid local OAuth completion URL")
	}
	return nil
}

func (p *Proxy) lockAuthorization(ctx context.Context, key string) (func(), error) {
	p.mu.Lock()
	gate := p.authGates[key]
	if gate == nil {
		if len(p.authGates) >= 128 {
			p.mu.Unlock()
			return nil, errors.New("MCP authorization capacity is full")
		}
		gate = &proxyAuthGate{ready: make(chan struct{}, 1)}
		p.authGates[key] = gate
	}
	gate.users++
	p.mu.Unlock()
	drop := func() {
		p.mu.Lock()
		gate.users--
		if gate.users == 0 {
			delete(p.authGates, key)
		}
		p.mu.Unlock()
	}
	select {
	case gate.ready <- struct{}{}:
		return func() { <-gate.ready; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// The probe only performs protocol discovery/initialization, never a tool call.
// A temporary legacy session created by the fallback is closed immediately.
func (p *Proxy) probeUpstream(ctx context.Context, target *url.URL, headers http.Header, token string) (*http.Request, *http.Response, error) {
	requests := []map[string]any{
		{"jsonrpc": "2.0", "id": "agentcfg-auth-probe", "method": "server/discover", "params": map[string]any{"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    wire.ModernProtocolVersion,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "agentcfg", "version": "1"}}}},
		{"jsonrpc": "2.0", "id": "agentcfg-auth-probe", "method": "initialize", "params": map[string]any{
			"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "agentcfg", "version": "1"}}},
	}
	for index, payload := range requests {
		data, _ := json.Marshal(payload)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(data))
		if err != nil {
			return nil, nil, errors.New("MCP authorization probe could not be created")
		}
		request.Header = headers.Clone()
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Content-Type", "application/json")
		if index == 0 {
			request.Header.Set("MCP-Protocol-Version", wire.ModernProtocolVersion)
			request.Header.Set("Mcp-Method", "server/discover")
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := p.client.Do(request)
		if err != nil {
			return nil, nil, errors.New("MCP upstream authorization probe failed")
		}
		if index == 0 && (response.StatusCode == 400 || response.StatusCode == 404 || response.StatusCode == 405) {
			_ = response.Body.Close()
			continue
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			_ = response.Body.Close()
			return nil, nil, errors.New("MCP upstream authorization redirect was rejected")
		}
		if session := response.Header.Get("Mcp-Session-Id"); session != "" && response.StatusCode >= 200 && response.StatusCode < 300 {
			_ = response.Body.Close()
			response.Body = io.NopCloser(strings.NewReader(""))
			p.closeProbeSession(ctx, target, headers, token, session)
		}
		return request, response, nil
	}
	return nil, nil, errors.New("MCP upstream authorization probe is unsupported")
}

func (p *Proxy) closeProbeSession(ctx context.Context, target *url.URL, headers http.Header, token, session string) {
	if !proxyOpaqueHeader(session, 4096) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, target.String(), nil)
	if err != nil {
		return
	}
	request.Header = headers.Clone()
	request.Header.Set("Mcp-Session-Id", session)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if response, err := p.client.Do(request); err == nil {
		_ = response.Body.Close()
	}
}
