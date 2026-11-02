package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"agentmux/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// Options does not open a browser or listener. Interactive callers supply the
// SDK fetcher, which returns the complete authorization response (code/state/iss).
// HTTPClient is for OAuth endpoints ONLY; do not pass a client that injects MCP
// resource headers, bearer tokens, or cookies into arbitrary destinations.
type Options struct {
	Home                     string
	Store                    Store
	HTTPClient               *http.Client
	AuthorizationCodeFetcher sdkauth.AuthorizationCodeFetcher
}

// CredentialKey separates server instances, resource URLs, accounts and OAuth
// client/security settings. Account is a caller-selected namespace, not a claim
// about the authenticated subject. It must change when using another account.
func CredentialKey(server config.Server) string {
	value := struct {
		Name    string
		Source  string
		URL     string
		Headers map[string]string
		OAuth   *config.OAuthConfig
	}{server.Name, server.Source, server.URL, server.Headers, server.OAuth}
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// NewHandler implements the official SDK OAuthHandler interface. Disabled
// OAuth returns (nil,nil) without inspecting the home directory or environment.
// Without a fetcher it can use/refresh stored tokens but returns ErrLoginRequired
// before discovery or dynamic registration when interactive consent is needed.
func NewHandler(ctx context.Context, server config.Server, opts Options) (sdkauth.OAuthHandler, error) {
	return newHandler(ctx, server, opts, true)
}

func newHandler(ctx context.Context, server config.Server, opts Options, useCachedToken bool) (sdkauth.OAuthHandler, error) {
	if server.OAuth == nil {
		return nil, nil
	}
	if err := validateServer(server); err != nil {
		return nil, err
	}
	store, locker, err := resolveStore(opts)
	if err != nil {
		return nil, err
	}
	client, err := newHTTPClient(server, opts.HTTPClient)
	if err != nil {
		return nil, err
	}
	key := CredentialKey(server)
	record, err := loadRecord(ctx, store, key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	scopes := &scopeState{}
	if record != nil {
		scopes.set(record.Scopes)
	}
	h := &handler{interactive: opts.AuthorizationCodeFetcher != nil, flow: make(chan struct{}, 1)}
	fetcher := func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		if opts.AuthorizationCodeFetcher == nil {
			return nil, ErrLoginRequired
		}
		if args == nil {
			return nil, fmt.Errorf("OAuth provider returned no authorization URL")
		}
		if err := allowedURL(args.URL, server); err != nil {
			return nil, err
		}
		authorizationURL, _ := url.Parse(args.URL)
		resource := authorizationURL.Query()["resource"]
		if len(resource) != 1 || resource[0] == "" {
			return nil, fmt.Errorf("SDK authorization URL has no unique resource indicator")
		}
		h.authorizationResource = resource[0]
		return opts.AuthorizationCodeFetcher(ctx, args)
	}
	sdkConfig := &sdkauth.AuthorizationCodeHandlerConfig{
		RedirectURL:              server.OAuth.RedirectURL,
		AuthorizationCodeFetcher: fetcher,
		Client:                   client,
		RequestRefreshToken:      true,
		AcceptUnadvertisedIss:    server.OAuth.Issuer != "",
		ScopeFilter: func(discovered []string) []string {
			if len(server.OAuth.Scopes) > 0 {
				return unionScopes(server.OAuth.Scopes, scopes.get())
			}
			return unionScopes(server.OAuth.Scopes, scopes.get(), discovered)
		},
		NewTokenSource: func(tokenContext context.Context, cfg *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			if cfg == nil || token == nil || token.AccessToken == "" {
				return nil, fmt.Errorf("OAuth provider returned no access token")
			}
			record := newRecord(key, cfg, token, grantedScopes(token, cfg.Scopes), h.authorizationResource, server.OAuth.Issuer)
			persistContext, cancel := context.WithTimeout(tokenContext, 45*time.Second)
			defer cancel()
			unlock, err := locker.Lock(persistContext, key)
			if err != nil {
				return nil, err
			}
			defer unlock()
			if err = saveRecord(persistContext, store, key, record); err != nil {
				return nil, err
			}
			scopes.set(record.Scopes)
			h.persisted.Store(true)
			// The SDK supplies a context with its OAuth HTTP client and a
			// lifetime suitable for later refreshes; preserve it.
			return managedTokenSource(tokenContext, store, locker, key, scopes, server.OAuth), nil
		},
	}
	if err = registration(sdkConfig, server.OAuth); err != nil {
		return nil, err
	}
	if record != nil && useCachedToken {
		tokenContext := context.WithValue(context.Background(), oauth2.HTTPClient, client)
		sdkConfig.InitialTokenSource = managedTokenSource(tokenContext, store, locker, key, scopes, server.OAuth)
	}
	h.sdk, err = sdkauth.NewAuthorizationCodeHandler(sdkConfig)
	if err != nil {
		return nil, safeError("construct OAuth handler", err)
	}
	return h, nil
}

func registration(sdk *sdkauth.AuthorizationCodeHandlerConfig, cfg *config.OAuthConfig) error {
	// The SDK tries CIMD before preregistration when both are set. Select the
	// explicit client first so a configured identity cannot silently change.
	if cfg.ClientID != "" {
		credentials := &oauthex.ClientCredentials{ClientID: cfg.ClientID, Issuer: cfg.Issuer}
		if cfg.ClientSecretEnv != "" {
			secret, ok := os.LookupEnv(cfg.ClientSecretEnv)
			if !ok || secret == "" {
				return fmt.Errorf("OAuth client secret environment variable %s is not set", cfg.ClientSecretEnv)
			}
			credentials.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: secret}
		}
		sdk.PreregisteredClient = credentials
	} else if cfg.ClientMetadataURL != "" {
		sdk.ClientIDMetadataDocumentConfig = &sdkauth.ClientIDMetadataDocumentConfig{URL: cfg.ClientMetadataURL}
	}
	// A pinned preregistered identity must not fall back to a different issuer's
	// dynamically registered client. This also keeps cached secret provenance
	// unambiguous when a clientSecretEnv value later rotates.
	if cfg.DynamicRegistration && cfg.ClientID == "" {
		applicationType := "web"
		redirect, _ := url.Parse(cfg.RedirectURL)
		if redirect.Scheme == "http" {
			applicationType = "native"
		}
		sdk.DynamicClientRegistrationConfig = &sdkauth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				RedirectURIs:            []string{cfg.RedirectURL},
				TokenEndpointAuthMethod: "none",
				GrantTypes:              []string{"authorization_code", "refresh_token"},
				ResponseTypes:           []string{"code"},
				ClientName:              "agentmux",
				Scope:                   strings.Join(cfg.Scopes, " "),
				ApplicationType:         applicationType,
			},
		}
	}
	return nil
}

func managedTokenSource(ctx context.Context, store Store, locker Locker, key string, scopes *scopeState, policy *config.OAuthConfig) *tokenSource {
	return &tokenSource{ctx: ctx, store: store, locker: locker, key: key, scopes: scopes, gate: make(chan struct{}, 1),
		clientID: policy.ClientID, issuer: policy.Issuer, secretEnv: policy.ClientSecretEnv}
}

func validateServer(server config.Server) error {
	if server.OAuth == nil {
		return fmt.Errorf("OAuth is not configured for this server")
	}
	if server.Type != "streamable-http" {
		return fmt.Errorf("OAuth requires a streamable-http server")
	}
	if err := server.OAuth.Validate(); err != nil {
		return err
	}
	if err := allowedURL(server.URL, server); err != nil {
		return fmt.Errorf("invalid OAuth resource URL: %w", err)
	}
	for name := range server.Headers {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") {
			return fmt.Errorf("OAuth cannot be combined with a literal authorization header")
		}
	}
	return nil
}

func resolveStore(opts Options) (Store, Locker, error) {
	store := opts.Store
	var err error
	if store == nil {
		store, err = NewSystemStore(opts.Home)
	}
	if err != nil {
		return nil, nil, err
	}
	locker, ok := store.(Locker)
	if !ok {
		return nil, nil, fmt.Errorf("OAuth Store must implement Locker for safe refresh-token rotation")
	}
	return store, locker, nil
}

type handler struct {
	sdk                   *sdkauth.AuthorizationCodeHandler
	interactive           bool
	flow                  chan struct{}
	persisted             atomic.Bool
	authorizationResource string
}

func (h *handler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	source, err := h.sdk.TokenSource(ctx)
	if managed, ok := source.(*tokenSource); ok {
		return &requestTokenSource{source: managed, ctx: ctx}, err
	}
	return source, err
}

func (h *handler) Authorize(ctx context.Context, request *http.Request, response *http.Response) error {
	if response == nil || response.Body == nil || request == nil || request.URL == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return fmt.Errorf("OAuth authorization requires an HTTP challenge")
	}
	if !h.interactive {
		_ = response.Body.Close()
		return ErrLoginRequired
	}
	select {
	case h.flow <- struct{}{}:
		defer func() { <-h.flow }()
	case <-ctx.Done():
		_ = response.Body.Close()
		return ctx.Err()
	}
	if err := h.sdk.Authorize(ctx, request, response); err != nil {
		if errors.Is(err, ErrLoginRequired) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return safeError("OAuth authorization", err)
	}
	return nil
}

type operationError struct {
	operation string
	cause     error
}

func (e *operationError) Error() string { return e.operation + " failed" }
func (e *operationError) Unwrap() error { return e.cause }

// Provider bodies can contain access tokens, codes or client secrets. Retain
// error identity for errors.Is/As while keeping ordinary diagnostics redacted.
func safeError(operation string, cause error) error { return &operationError{operation, cause} }
