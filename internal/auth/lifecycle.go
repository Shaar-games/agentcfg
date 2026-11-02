package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"agentmux/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Status is a local inspection, not a remote token-validity or identity claim.
// Authenticated means the stored access token has not expired (including the
// oauth2 refresh safety window). CanRefresh only indicates a stored refresh token.
type Status struct {
	Enabled       bool
	Stored        bool
	Authenticated bool
	CanRefresh    bool
	Account       string
	Expiry        time.Time
	Scopes        []string
}

// Inspect does no networking, refresh, directory creation or login.
func Inspect(ctx context.Context, server config.Server, opts Options) (Status, error) {
	result := Status{Enabled: server.OAuth != nil}
	if server.OAuth == nil {
		return result, nil
	}
	result.Account = server.OAuth.Account
	if err := validateServer(server); err != nil {
		return result, err
	}
	store, _, err := resolveStore(opts)
	if err != nil {
		return result, err
	}
	record, err := loadRecord(ctx, store, CredentialKey(server))
	if errors.Is(err, ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Stored = true
	result.Authenticated = !record.RefreshInProgress && record.Token.Valid()
	result.CanRefresh = !record.RefreshInProgress && record.Token.RefreshToken != ""
	result.Expiry = record.Token.Expiry
	result.Scopes = append([]string(nil), record.Scopes...)
	return result, nil
}

// Logout removes only this account/server's locally cached credentials. It
// does not claim to revoke a grant at the provider. Existing token sources
// notice deletion on their next Token call.
func Logout(ctx context.Context, server config.Server, opts Options) error {
	if server.OAuth == nil {
		return nil
	}
	if err := validateServer(server); err != nil {
		return err
	}
	store, locker, err := resolveStore(opts)
	if err != nil {
		return err
	}
	key := CredentialKey(server)
	// A missing store should not be created by a logout command.
	value, err := store.Load(ctx, key)
	clear(value)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	unlock, err := locker.Lock(ctx, key)
	if err != nil {
		return err
	}
	defer unlock()
	return store.Delete(ctx, key)
}

// Login initializes a real MCP connection through the official SDK. It succeeds
// only after credentials are securely persisted and MCP initialization succeeds.
// The caller implements the explicit browser/manual callback UI using Fetcher;
// this package never creates an HTTP listener or opens a browser.
func Login(ctx context.Context, server config.Server, opts Options) error {
	if opts.AuthorizationCodeFetcher == nil {
		return ErrLoginRequired
	}
	// Explicit login starts a fresh authorization, retaining the existing
	// encrypted record until a replacement grant has been safely persisted.
	h, err := newHandler(ctx, server, opts, false)
	if err != nil {
		return err
	}
	if h == nil {
		return fmt.Errorf("OAuth is not configured for this server")
	}
	client, err := newHTTPClient(server, opts.HTTPClient)
	if err != nil {
		return err
	}
	u, _ := url.Parse(server.URL)
	client.Transport = &resourceTransport{next: client.Transport, origin: u.Scheme + "://" + u.Host, headers: server.Headers}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "agentmux-auth", Version: "1"}, nil)
	session, err := mcpClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: client, OAuthHandler: h}, nil)
	if err != nil {
		return safeError("OAuth MCP login", err)
	}
	closeErr := session.Close()
	status, err := Inspect(ctx, server, opts)
	if err != nil {
		return err
	}
	if !h.(*handler).persisted.Load() || !status.Stored {
		return fmt.Errorf("server completed MCP initialization without a new OAuth grant; no new login was recorded")
	}
	if closeErr != nil {
		return safeError("close OAuth MCP login session", closeErr)
	}
	return nil
}

// ParseAuthorizationResponse validates a caller-supplied complete redirect URL.
// The official SDK subsequently validates state and issuer against its flow.
// A bare code is insufficient: callers must preserve state and optional iss.
func ParseAuthorizationResponse(redirectURL, responseURL string) (*sdkauth.AuthorizationResult, error) {
	if err := allowedURL(redirectURL, config.Server{URL: redirectURL}); err != nil {
		return nil, fmt.Errorf("invalid configured OAuth redirect URL")
	}
	if len(responseURL) > 16384 {
		return nil, fmt.Errorf("OAuth response URL exceeds size limit")
	}
	expected, err := url.Parse(redirectURL)
	if err != nil || expected.Host == "" || expected.RawQuery != "" || expected.ForceQuery || expected.Fragment != "" || expected.User != nil {
		return nil, fmt.Errorf("invalid configured OAuth redirect URL")
	}
	actual, err := url.Parse(responseURL)
	if err != nil || actual.User != nil || strings.Contains(responseURL, "#") ||
		actual.Scheme != expected.Scheme || !strings.EqualFold(actual.Host, expected.Host) ||
		actual.EscapedPath() != expected.EscapedPath() || actual.Opaque != "" {
		return nil, fmt.Errorf("OAuth response URL does not match the configured redirect")
	}
	query, err := url.ParseQuery(actual.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("invalid OAuth response query")
	}
	for _, name := range []string{"code", "state", "iss", "error", "error_description"} {
		if len(query[name]) > 1 {
			return nil, fmt.Errorf("OAuth response repeats %s", name)
		}
	}
	if query.Has("error") {
		return nil, fmt.Errorf("OAuth provider declined authorization")
	}
	if query.Get("code") == "" || query.Get("state") == "" {
		return nil, fmt.Errorf("OAuth response requires code and state")
	}
	return &sdkauth.AuthorizationResult{Code: query.Get("code"), State: query.Get("state"), Iss: query.Get("iss")}, nil
}
