// Package auth provides OAuth authorization-code login and credential
// persistence for streamable-http MCP upstreams using
// github.com/modelcontextprotocol/go-sdk/auth and oauthex.
//
// # Integration
//
// NewHandler is assigned to mcp.StreamableClientTransport.OAuthHandler.
// The default handler is noninteractive: it restores/refreshes cached
// credentials, or returns ErrLoginRequired before discovery/registration.
// Login runs a real MCP initialization with an explicitly supplied SDK
// AuthorizationCodeFetcher. The caller displays/opens args.URL and returns the
// complete callback code, state and optional issuer. ParseAuthorizationResponse
// validates a manually supplied redirect URL; the SDK validates the flow's
// state, issuer and S256 PKCE. This package creates no listener or browser.
//
// Inspect reads local state without refreshing or checking the provider.
// Logout removes this server/account's cached credentials, not the remote grant.
// Login starts fresh authorization while preserving any old record until a
// replacement has been safely persisted. A failed MCP initialization is never
// reported as a successful login, even if the token exchange already succeeded.
//
// # Configuration
//
// OAuth is an agentmux extension in canonical .agentmux/mcp.json server.oauth or
// .agentmux/agentmux.json serverOverrides.<name>.oauth. It is never a portable
// Agent Plugins mcp.json field. config.OAuthConfig accepts account, clientId,
// issuer, clientSecretEnv, clientMetadataUrl, redirectUrl, scopes and
// dynamicRegistration. DCR requires explicit opt-in; an explicitly configured
// client ID takes priority over client metadata URLs and DCR. Client secrets can only
// be named through an environment variable and require a pinned issuer. Refresh
// reads that variable again, after matching the cached client and pinned issuer.
// Explicit scopes bound requested scopes; omitted scopes use discovery.
// A matching unadvertised response issuer is accepted only for a pinned issuer.
//
// The account label partitions credentials and broker configurations; it does
// not verify a provider's subject. Use a distinct label for each desired
// identity. Keys also include server name, source, resource URL, headers and
// OAuth client/security settings. Tokens and client credentials are encrypted.
//
// # Secure storage
//
// NewSystemStore uses per-user Windows DPAPI to wrap a fresh AES-256-GCM key for
// each record. No machine-wide DPAPI scope is used. An explicitly configured
// AGENTMUX_OAUTH_KEY overrides this selection: it must be the base64 encoding of
// 32 cryptographically random bytes supplied by an external secret manager.
// NewEncryptedFileStore exposes that alternative directly. The external key
// is never saved or derived from a user name, machine identifier or password.
// Unix has no built-in native keychain adapter in this version; without an
// explicit key or caller-supplied secure Store, it returns
// ErrSecureStoreUnavailable. There is no plaintext fallback.
//
// Records live under <home>/.agentmux/credentials/oauth with private
// permissions, atomic ciphertext replacement, and identity-bound encryption.
// Construction, absent-record reads and Inspect do not create directories.
// Store implementations supplied to NewHandler must also implement Locker:
// locking covers reload, refresh, rotated-token persistence and logout.
// Absolute expiry survives restart; a changed refresh token is saved even if
// the access token stays the same. The SDK-discovered resource indicator is
// retained in refresh requests through the SDK's token-source customization.
// A persisted refresh-in-progress marker prevents peers from replaying an old
// refresh token after a crash or a failed replacement save. The original source
// can retry persistence of its received replacement; otherwise an interrupted
// refresh requires a new login, including when the network outcome is unknown.
// Corrupt or wrong-key records are not silently
// overwritten/deleted. Lock files remain after logout to avoid lock-file races.
// Existing Windows directories are not modified or ACL-audited by reads;
// confidentiality is cryptographic, and preventing ciphertext deletion/replay
// also relies on the preexisting home-directory access policy.
//
// # Networking and boundaries
//
// OAuth uses a dedicated client, without MCP resource headers or a shared
// cookie jar, with finite timeouts and redirects disabled. Its default
// transport connects directly and rejects unconfigured non-public destinations
// at DNS resolution.
// Explicit resource and pinned issuer hosts are trusted for private services.
// A supplied HTTPClient owns its transport/DNS policy and must not inject
// credentials into other endpoints. Each operation's cancellation applies to
// refresh and lock acquisition. Do not pass an MCP header-injecting client.
//
// Authorization-code discovery/registration/PKCE/state/token exchange comes
// from SDK v1.8.0. Device authorization, provider token revocation, native Unix
// keychains and automatic account-subject verification are not implemented.
// Tests use temporary homes and synthetic providers; they do not perform a
// real login or invoke DPAPI against the user's profile.
//
// References:
//   - https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/auth
//   - https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/oauthex
//   - https://agent-plugins.org/plugin-authors/mcp-servers
//   - https://learn.microsoft.com/windows/win32/api/dpapi/nf-dpapi-cryptprotectdata
package auth
