package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	"agentcfg/internal/ipc"
	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// These tests exercise both production sides of the private IPC connection.
// The fake browser completes a real local authorization-code/PKCE exchange;
// it never opens a browser or uses an account or credential outside t.TempDir.
type oauthStdioFixture struct {
	t                *testing.T
	http             *httptest.Server
	public           atomic.Bool
	dropTool         atomic.Bool
	browserCalls     atomic.Int32
	metadataRequests atomic.Int32
	registrations    atomic.Int32
	exchanges        atomic.Int32
	challenges       atomic.Int32
	initializes      atomic.Int32
	discovers        atomic.Int32
	discoveryDelay   atomic.Int64
	toolCalls        atomic.Int32
	mu               sync.Mutex
	validAccess      string
	grants           map[string]oauthStdioGrant
}

type oauthStdioGrant struct {
	challenge string
	redirect  string
}

func newOAuthStdioFixture(t *testing.T) *oauthStdioFixture {
	t.Helper()
	f := &oauthStdioFixture{t: t, grants: make(map[string]oauthStdioGrant)}
	f.http = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.http.Close)
	return f
}

func (f *oauthStdioFixture) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/mcp" {
		f.serveMCP(w, r)
		return
	}
	if r.Header.Get("X-MCP-Only") != "" || r.Header.Get("Authorization") != "" {
		f.t.Error("MCP resource credentials leaked into an OAuth request")
	}
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource":
		f.metadataRequests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource": f.http.URL + "/mcp", "authorization_servers": []string{f.http.URL},
			"scopes_supported": []string{"tools.read"},
		})
	case "/.well-known/oauth-authorization-server":
		f.metadataRequests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.http.URL, "authorization_endpoint": f.http.URL + "/authorize",
			"token_endpoint": f.http.URL + "/token", "registration_endpoint": f.http.URL + "/register",
			"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"},
			"authorization_response_iss_parameter_supported": true,
		})
	case "/register":
		var metadata struct {
			Redirects []string `json:"redirect_uris"`
			Method    string   `json:"token_endpoint_auth_method"`
		}
		if json.NewDecoder(r.Body).Decode(&metadata) != nil || len(metadata.Redirects) != 1 || metadata.Method != "none" {
			f.t.Error("invalid dynamic registration for the stdio OAuth client")
			http.Error(w, "invalid registration", http.StatusBadRequest)
			return
		}
		f.registrations.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id": "stdio-fixture-client", "redirect_uris": metadata.Redirects, "token_endpoint_auth_method": "none",
		})
	case "/authorize":
		query := r.URL.Query()
		if query.Get("state") == "" || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" ||
			query.Get("resource") != f.http.URL+"/mcp" || query.Get("client_id") != "stdio-fixture-client" {
			f.t.Error("authorization lost state, PKCE, resource or registered client binding")
			http.Error(w, "invalid authorization", http.StatusBadRequest)
			return
		}
		redirect, err := url.Parse(query.Get("redirect_uri"))
		if err != nil || redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" || redirect.Port() == "" {
			f.t.Error("automatic OAuth did not supply an explicit loopback callback")
			http.Error(w, "invalid callback", http.StatusBadRequest)
			return
		}
		code := "private-stdio-code-" + query.Get("state")
		f.mu.Lock()
		f.grants[code] = oauthStdioGrant{challenge: query.Get("code_challenge"), redirect: redirect.String()}
		f.mu.Unlock()
		redirect.RawQuery = url.Values{"code": {code}, "state": {query.Get("state")}, "iss": {f.http.URL}}.Encode()
		w.Header().Set("Location", redirect.String())
		w.WriteHeader(http.StatusFound)
	case "/token":
		if r.ParseForm() != nil || r.Form.Get("grant_type") != "authorization_code" {
			http.Error(w, "unexpected grant", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		grant, exists := f.grants[r.Form.Get("code")]
		delete(f.grants, r.Form.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !exists || grant.challenge != base64.RawURLEncoding.EncodeToString(sum[:]) || grant.redirect != r.Form.Get("redirect_uri") ||
			r.Form.Get("resource") != f.http.URL+"/mcp" || r.Form.Get("client_id") != "stdio-fixture-client" {
			f.t.Error("token exchange lost PKCE, callback, resource or client binding")
			http.Error(w, "invalid grant", http.StatusBadRequest)
			return
		}
		n := f.exchanges.Add(1)
		access := fmt.Sprintf("private-stdio-access-%d", n)
		f.mu.Lock()
		f.validAccess = access
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": access, "token_type": "Bearer", "refresh_token": fmt.Sprintf("private-stdio-refresh-%d", n), "expires_in": 3600,
		})
	default:
		http.NotFound(w, r)
	}
}

func (f *oauthStdioFixture) serveMCP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	access := f.validAccess
	f.mu.Unlock()
	if !f.public.Load() && (access == "" || r.Header.Get("Authorization") != "Bearer "+access) {
		f.challenges.Add(1)
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, f.http.URL+"/.well-known/oauth-protected-resource/mcp"))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
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
		http.Error(w, "invalid MCP", http.StatusBadRequest)
		return
	}
	if !message.HasID() {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch message.Method() {
	case "initialize":
		f.initializes.Add(1)
		result = map[string]any{
			"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]string{"name": "oauth-stdio-fixture", "version": "1"},
		}
	case "server/discover":
		f.discovers.Add(1)
		if !wire.IsModernRequest(message) || r.Header.Get("MCP-Protocol-Version") != wire.ModernProtocolVersion {
			f.t.Error("modern OAuth discovery lost its protocol metadata")
		}
		if delay := time.Duration(f.discoveryDelay.Load()); delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
		}
		result = map[string]any{"supportedVersions": []string{wire.ModernProtocolVersion}, "capabilities": map[string]any{"tools": map[string]any{}}}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{
			"name": "echo", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		}}}
	case "tools/call":
		f.toolCalls.Add(1)
		if f.dropTool.Load() {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				f.t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "authenticated stdio round trip"}}}
	default:
		_ = json.NewEncoder(w).Encode(wire.Error(message.ID(), -32601, "unknown fixture method"))
		return
	}
	_ = json.NewEncoder(w).Encode(wire.Result(message.ID(), result))
}

func (f *oauthStdioFixture) fetch(ctx context.Context, redirect string, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
	f.browserCalls.Add(1)
	if args == nil {
		return nil, errors.New("missing authorization arguments")
	}
	u, err := url.Parse(args.URL)
	if err != nil || u.Query().Get("redirect_uri") != redirect {
		return nil, errors.New("authorization callback differs from private IPC callback")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		return nil, fmt.Errorf("fixture authorization returned HTTP %d", response.StatusCode)
	}
	return auth.ParseAuthorizationResponse(redirect, response.Header.Get("Location"))
}

func oauthStdioHome(t *testing.T, endpoint, sharing string, headers map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR"} {
		t.Setenv(name, root)
	}
	// Unix-domain sockets may fall back to os.TempDir when the cache path is
	// long. Give that fallback its own short, test-owned temporary directory.
	socketTemp, err := os.MkdirTemp("", "ao-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketTemp) })
	for _, name := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(name, socketTemp)
	}
	t.Setenv("AGENTCFG_OAUTH_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6e}, 32)))
	if err := os.MkdirAll(filepath.Join(root, ".agentcfg"), 0o700); err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{"type": "streamable-http", "url": endpoint, "sharing": sharing}
	if headers != nil {
		entry["headers"] = headers
	}
	config := map[string]any{"mcpServers": map[string]any{"fixture": entry}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".agentcfg", "mcp.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func serveOAuthStdioBroker(t *testing.T, root string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- Serve(ctx, root, io.Discard) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("temporary OAuth broker did not stop")
		}
	})
	endpoint, err := ipc.ForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		connection, err := ipc.Dial(ctx, endpoint)
		if err == nil {
			_ = connection.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("temporary OAuth broker never became ready: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type oauthStdioCapture struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (c *oauthStdioCapture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.Write(data)
}

func (c *oauthStdioCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.String()
}

type oauthStdioClient struct {
	conn    net.Conn
	proxy   net.Conn
	reader  *wire.Reader
	writer  *wire.Writer
	capture *oauthStdioCapture
	cancel  context.CancelFunc
	done    chan error
	once    sync.Once
}

func connectOAuthStdio(t *testing.T, root string, fetch func(context.Context, string, *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error)) *oauthStdioClient {
	t.Helper()
	client, proxy := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	p := &oauthStdioClient{conn: client, proxy: proxy, reader: wire.NewReader(client), writer: wire.NewWriter(client),
		capture: &oauthStdioCapture{}, cancel: cancel, done: make(chan error, 1)}
	go func() {
		p.done <- ConnectWithOptions(ctx, root, "fixture", "cursor", proxy, io.MultiWriter(p.capture, proxy), ConnectOptions{AuthorizationCodeFetcher: fetch})
	}()
	t.Cleanup(func() { p.close(t) })
	return p
}

func (p *oauthStdioClient) close(t *testing.T) {
	t.Helper()
	p.once.Do(func() {
		p.cancel()
		_ = p.conn.Close()
		_ = p.proxy.Close()
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			t.Error("stdio proxy did not stop after the client disconnected")
		}
	})
}

func (p *oauthStdioClient) send(t *testing.T, method string, id any, params any) {
	t.Helper()
	_ = p.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := p.writer.Write(wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(id), "method": wire.Raw(method), "params": wire.Raw(params)}); err != nil {
		t.Fatal(err)
	}
}

func (p *oauthStdioClient) read(t *testing.T, id any, success bool) wire.Message {
	t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	message, err := p.reader.Read()
	if err != nil {
		t.Fatalf("read MCP from stdio: %v; output: %s", err, p.capture.String())
	}
	if !bytes.Equal(message.ID(), wire.Raw(id)) || string(message["jsonrpc"]) != `"2.0"` || message.Method() != "" {
		t.Fatalf("private OAuth control or uncorrelated response escaped to MCP stdio: %s", wire.Raw(message))
	}
	if success && message["error"] != nil {
		t.Fatalf("MCP %v failed: %s", id, message["error"])
	}
	if !success && message["error"] == nil {
		t.Fatalf("MCP %v unexpectedly succeeded: %s", id, message["result"])
	}
	return message
}

func (p *oauthStdioClient) beginInitialize(t *testing.T) {
	p.send(t, "initialize", "init", map[string]any{
		"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "stdio-oauth-client", "version": "1"},
	})
}

func (p *oauthStdioClient) finishInitialize(t *testing.T) {
	p.read(t, "init", true)
	if err := p.writer.Write(wire.Notification("notifications/initialized", nil)); err != nil {
		t.Fatal(err)
	}
}

func (p *oauthStdioClient) initialize(t *testing.T) {
	p.beginInitialize(t)
	p.finishInitialize(t)
}

func (p *oauthStdioClient) call(t *testing.T, id string, success bool) {
	p.send(t, "tools/call", id, map[string]any{"name": "echo", "arguments": map[string]any{}})
	p.read(t, id, success)
}

func assertOAuthStdioSecretsAbsent(t *testing.T, p *oauthStdioClient, fixtureURL string) {
	t.Helper()
	for _, secret := range []string{fixtureURL, "private-stdio-code-", "private-stdio-access-", "private-stdio-refresh-", "code_challenge", "redirect_uri"} {
		if strings.Contains(p.capture.String(), secret) {
			t.Errorf("private OAuth value %q leaked into MCP stdout", secret)
		}
	}
}

func TestOAuthStdioURLOnlyAuthenticatesAndReusesCredentials(t *testing.T) {
	f := newOAuthStdioFixture(t)
	root := oauthStdioHome(t, f.http.URL+"/mcp", "isolated", map[string]string{"X-MCP-Only": "private-resource-header"})
	configPath := filepath.Join(root, ".agentcfg", "mcp.json")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	serveOAuthStdioBroker(t, root)
	first := connectOAuthStdio(t, root, f.fetch)
	first.initialize(t)
	first.send(t, "tools/list", "list", map[string]any{})
	if result := first.read(t, "list", true); !strings.Contains(string(result["result"]), `"echo"`) {
		t.Fatal("authenticated tool catalog did not reach stdio")
	}
	first.call(t, "first", true)
	first.close(t)
	// Isolated backends are destroyed on detach; this connection must reload the
	// encrypted grant rather than inherit the first transport's token in memory.
	second := connectOAuthStdio(t, root, f.fetch)
	second.initialize(t)
	second.call(t, "second", true)
	if f.browserCalls.Load() != 1 || f.exchanges.Load() != 1 || f.registrations.Load() != 1 {
		t.Fatalf("cached grant triggered another login: browsers=%d exchanges=%d registrations=%d", f.browserCalls.Load(), f.exchanges.Load(), f.registrations.Load())
	}
	assertOAuthStdioSecretsAbsent(t, first, f.http.URL)
	assertOAuthStdioSecretsAbsent(t, second, f.http.URL)
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("runtime OAuth modified the canonical MCP configuration")
	}
	records, err := filepath.Glob(filepath.Join(root, ".agentcfg", "credentials", "oauth", "*.json"))
	if err != nil || len(records) == 0 {
		t.Fatal("successful OAuth did not persist an encrypted credential record")
	}
	for _, path := range records {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("private-stdio-")) {
			t.Fatal("credential record contains plaintext fixture credentials")
		}
	}
}

func TestOAuthStdioConcurrentClientsShareInteractiveLogin(t *testing.T) {
	for _, sharing := range []string{"shared", "isolated"} {
		t.Run(sharing, func(t *testing.T) {
			f := newOAuthStdioFixture(t)
			root := oauthStdioHome(t, f.http.URL+"/mcp", sharing, nil)
			serveOAuthStdioBroker(t, root)
			started, release := make(chan struct{}, 2), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			fetch := func(ctx context.Context, redirect string, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
				select {
				case started <- struct{}{}:
				default:
				}
				select {
				case <-release:
					return f.fetch(ctx, redirect, args)
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			first, second := connectOAuthStdio(t, root, fetch), connectOAuthStdio(t, root, fetch)
			first.beginInitialize(t)
			second.beginInitialize(t)
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("concurrent stdio clients never started OAuth")
			}
			unblock()
			first.finishInitialize(t)
			second.finishInitialize(t)
			first.call(t, "first", true)
			second.call(t, "second", true)
			if f.browserCalls.Load() != 1 || f.exchanges.Load() != 1 {
				t.Fatalf("concurrent clients opened multiple OAuth flows: browsers=%d exchanges=%d", f.browserCalls.Load(), f.exchanges.Load())
			}
		})
	}
}

func TestOAuthStdioReauthorizesAfterRejectedToken(t *testing.T) {
	f := newOAuthStdioFixture(t)
	root := oauthStdioHome(t, f.http.URL+"/mcp", "shared", nil)
	serveOAuthStdioBroker(t, root)
	p := connectOAuthStdio(t, root, f.fetch)
	p.initialize(t)
	p.call(t, "before-revocation", true)
	f.mu.Lock()
	f.validAccess = ""
	f.mu.Unlock()
	p.call(t, "after-revocation", true)
	if f.browserCalls.Load() != 2 || f.exchanges.Load() != 2 || f.toolCalls.Load() != 2 {
		t.Fatalf("401 did not reauthorize exactly once: browsers=%d exchanges=%d tools=%d", f.browserCalls.Load(), f.exchanges.Load(), f.toolCalls.Load())
	}
	assertOAuthStdioSecretsAbsent(t, p, f.http.URL)
}

func TestOAuthStdioDisconnectCancelsLogin(t *testing.T) {
	f := newOAuthStdioFixture(t)
	root := oauthStdioHome(t, f.http.URL+"/mcp", "shared", nil)
	serveOAuthStdioBroker(t, root)
	started, stopped := make(chan struct{}), make(chan struct{})
	p := connectOAuthStdio(t, root, func(ctx context.Context, _ string, _ *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	})
	p.beginInitialize(t)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("stdio client did not receive the OAuth request")
	}
	// Closing the client's streams must cancel the browser callback even when
	// the outer command context itself has not been cancelled.
	_ = p.conn.Close()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnected stdio client left its authorization callback running")
	}
	p.close(t)
	next := connectOAuthStdio(t, root, f.fetch)
	next.initialize(t)
	next.call(t, "after-cancellation", true)
	if f.exchanges.Load() != 1 {
		t.Fatalf("cancelled authorization unexpectedly exchanged tokens: %d", f.exchanges.Load())
	}
}

func TestOAuthStdioOwnerDisconnectKeepsOtherClient(t *testing.T) {
	f := newOAuthStdioFixture(t)
	root := oauthStdioHome(t, f.http.URL+"/mcp", "isolated", nil)
	serveOAuthStdioBroker(t, root)
	started := make(chan struct{}, 1)
	first := connectOAuthStdio(t, root, func(ctx context.Context, _ string, _ *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	first.beginInitialize(t)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first client never started authorization")
	}
	second := connectOAuthStdio(t, root, f.fetch)
	second.beginInitialize(t)
	// Both independent HTTP transports must have encountered the auth challenge
	// before the client hosting the first browser interaction disappears.
	deadline := time.Now().Add(5 * time.Second)
	for f.challenges.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("second isolated client never reached the OAuth challenge")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if status, err := GetStatus(ctx, root); err != nil || len(status.Backends) != 2 {
		t.Fatalf("both isolated clients did not attach before disconnection: %v, %+v", err, status)
	}
	_ = first.conn.Close()
	second.finishInitialize(t)
	second.call(t, "surviving-client", true)
	if f.exchanges.Load() != 1 {
		t.Fatalf("surviving client did not complete one new grant: %d", f.exchanges.Load())
	}
}

func TestOAuthStdioPublicServerNeedsNoCredentialStore(t *testing.T) {
	f := newOAuthStdioFixture(t)
	f.public.Store(true)
	root := oauthStdioHome(t, f.http.URL+"/mcp", "shared", nil)
	// Even a broken store setting must not make an unauthenticated public MCP
	// depend on OAuth, and no credential directory should be created on its path.
	t.Setenv("AGENTCFG_OAUTH_KEY", "deliberately-invalid-unused-key")
	serveOAuthStdioBroker(t, root)
	p := connectOAuthStdio(t, root, f.fetch)
	p.initialize(t)
	p.call(t, "public", true)
	if f.browserCalls.Load() != 0 || f.metadataRequests.Load() != 0 {
		t.Fatal("public MCP unexpectedly started OAuth discovery or a browser")
	}
	if _, err := os.Stat(filepath.Join(root, ".agentcfg", "credentials")); !os.IsNotExist(err) {
		t.Fatalf("public MCP touched OAuth credential storage: %v", err)
	}
}

func TestOAuthStdioPreservesExplicitAuthorization(t *testing.T) {
	f := newOAuthStdioFixture(t)
	root := oauthStdioHome(t, f.http.URL+"/mcp", "shared", map[string]string{"Authorization": "Bearer private-user-supplied-token"})
	serveOAuthStdioBroker(t, root)
	p := connectOAuthStdio(t, root, f.fetch)
	p.beginInitialize(t)
	p.read(t, "init", false)
	if f.browserCalls.Load() != 0 || f.metadataRequests.Load() != 0 || f.exchanges.Load() != 0 {
		t.Fatal("an explicitly configured bearer token was replaced by automatic OAuth")
	}
	if strings.Contains(p.capture.String(), "private-user-supplied-token") {
		t.Fatal("explicit authorization header leaked into MCP stdout")
	}
}

func TestOAuthStdioDoesNotReplayToolAfterNetworkFailure(t *testing.T) {
	f := newOAuthStdioFixture(t)
	root := oauthStdioHome(t, f.http.URL+"/mcp", "shared", nil)
	serveOAuthStdioBroker(t, root)
	p := connectOAuthStdio(t, root, f.fetch)
	p.initialize(t)
	f.dropTool.Store(true)
	p.call(t, "uncertain-outcome", false)
	if f.toolCalls.Load() != 1 || f.browserCalls.Load() != 1 {
		t.Fatalf("network failure replayed a tool or opened another login: tools=%d browsers=%d", f.toolCalls.Load(), f.browserCalls.Load())
	}
	assertOAuthStdioSecretsAbsent(t, p, f.http.URL)
}

func TestOAuthStdioModernDiscoveryWaitsForBrowser(t *testing.T) {
	testOAuthStdioModernDiscovery(t, 5200*time.Millisecond, 0)
}

func TestOAuthStdioModernDiscoveryKeepsBudgetAfterConsent(t *testing.T) {
	// At the five-second probe deadline OAuth has already finished, but its
	// authenticated retry still owns the normal request budget.
	testOAuthStdioModernDiscovery(t, 4800*time.Millisecond, 500*time.Millisecond)
}

func testOAuthStdioModernDiscovery(t *testing.T, browserDelay, responseDelay time.Duration) {
	t.Helper()
	f := newOAuthStdioFixture(t)
	f.discoveryDelay.Store(int64(responseDelay))
	root := oauthStdioHome(t, f.http.URL+"/mcp", "shared", nil)
	serveOAuthStdioBroker(t, root)
	p := connectOAuthStdio(t, root, func(ctx context.Context, redirect string, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		// Discovery normally abandons an unresponsive server after five seconds.
		// An active user authorization must get the normal request budget instead.
		timer := time.NewTimer(browserDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
			return f.fetch(ctx, redirect, args)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	_ = p.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := p.writer.Write(modernMessage("discover", "server/discover", nil)); err != nil {
		t.Fatal(err)
	}
	p.read(t, "discover", true)
	_ = p.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := p.writer.Write(modernMessage("catalog", "tools/list", nil)); err != nil {
		t.Fatal(err)
	}
	p.read(t, "catalog", true)
	if err := p.writer.Write(modernMessage("modern-tool", "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}})); err != nil {
		t.Fatal(err)
	}
	p.read(t, "modern-tool", true)
	if f.browserCalls.Load() != 1 || f.exchanges.Load() != 1 || f.initializes.Load() != 0 || f.discovers.Load() != 1 || f.toolCalls.Load() != 1 {
		t.Fatalf("OAuth lost modern discovery or replayed work: browsers=%d exchanges=%d initialize=%d discover=%d tools=%d",
			f.browserCalls.Load(), f.exchanges.Load(), f.initializes.Load(), f.discovers.Load(), f.toolCalls.Load())
	}
	assertOAuthStdioSecretsAbsent(t, p, f.http.URL)
}
