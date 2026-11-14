package broker

import (
	"bytes"
	"context"
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
	"agentcfg/internal/config"
	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

const elicitationAuthTool = "agentcfg_authenticate"

type elicitationCallbackResult struct {
	result *sdkauth.AuthorizationResult
	err    error
}

// This harness supplies a real loopback callback and a local OAuth provider.
// Opening a URL means making an HTTP request to those two fixtures only.
type oauthElicitationHarness struct {
	root       string
	server     config.Server
	fixture    *oauthStdioFixture
	callback   *httptest.Server
	clientUI   atomic.Int32
	directUI   atomic.Int32
	openedURLs atomic.Int32
	mu         sync.Mutex
	pending    chan elicitationCallbackResult
}

type oauthElicitationFixtureOptions struct {
	sharing       string
	legacyOnly    bool
	legacyVersion string
	public        bool
}

func newOAuthElicitationHarness(t *testing.T, autoOpen *bool, options ...oauthElicitationFixtureOptions) *oauthElicitationHarness {
	t.Helper()
	settings := oauthElicitationFixtureOptions{sharing: "isolated"}
	if len(options) != 0 {
		settings = options[0]
		if settings.sharing == "" {
			settings.sharing = "isolated"
		}
	}
	h := &oauthElicitationHarness{fixture: newOAuthStdioFixture(t)}
	h.fixture.public.Store(settings.public)
	if settings.legacyOnly || settings.legacyVersion != "" {
		useLegacyElicitationProvider(t, h.fixture, settings)
	}
	h.root = oauthStdioHome(t, h.fixture.http.URL+"/mcp", settings.sharing, nil)
	h.callback = httptest.NewServer(http.HandlerFunc(h.receiveCallback))
	t.Cleanup(h.callback.Close)
	cfg, err := config.Load(h.root)
	if err != nil {
		t.Fatal(err)
	}
	server := auth.InteractiveServer(cfg.Servers["fixture"])
	server.OAuth.RedirectURL = h.callback.URL + "/oauth/callback"
	encoded, err := json.Marshal(map[string]any{"mcpServers": map[string]config.Server{"fixture": server}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.root, ".agentcfg", "mcp.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if autoOpen != nil {
		settings, err := json.Marshal(map[string]any{"oauth": map[string]bool{"autoOpenBrowser": *autoOpen}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.root, ".agentcfg", "agentcfg.json"), settings, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err = config.Load(h.root)
	if err != nil {
		t.Fatal(err)
	}
	h.server = cfg.Servers["fixture"]
	serveOAuthStdioBroker(t, h.root)
	return h
}

func useLegacyElicitationProvider(t *testing.T, fixture *oauthStdioFixture, settings oauthElicitationFixtureOptions) {
	t.Helper()
	fixture.http.Close()
	fixture.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" && r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			var message wire.Message
			if json.Unmarshal(body, &message) == nil {
				fixture.mu.Lock()
				access := fixture.validAccess
				fixture.mu.Unlock()
				if fixture.public.Load() || (access != "" && r.Header.Get("Authorization") == "Bearer "+access) {
					if message.Method() == "server/discover" && settings.legacyOnly {
						fixture.discovers.Add(1)
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(wire.Error(message.ID(), -32601, "fixture supports legacy initialization only"))
						return
					}
					if message.Method() == "initialize" && settings.legacyVersion != "" {
						fixture.initializes.Add(1)
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(wire.Result(message.ID(), map[string]any{
							"protocolVersion": settings.legacyVersion, "capabilities": map[string]any{"tools": map[string]any{}},
							"serverInfo": map[string]string{"name": "oauth-legacy-version-fixture", "version": "1"},
						}))
						return
					}
				}
			}
		}
		// For a protected resource the initial 401 intentionally hides whether
		// server/discover is supported until the OAuth exchange has completed.
		fixture.serve(w, r)
	}))
	t.Cleanup(fixture.http.Close)
}

func (h *oauthElicitationHarness) receiveCallback(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	pending := h.pending
	h.mu.Unlock()
	if pending == nil || r.Method != http.MethodGet || r.URL.Path != "/oauth/callback" {
		http.Error(w, "no pending fixture authorization", http.StatusGone)
		return
	}
	result, err := auth.ParseAuthorizationResponse(h.callback.URL+"/oauth/callback", h.callback.URL+r.URL.RequestURI())
	select {
	case pending <- elicitationCallbackResult{result: result, err: err}:
	default:
		http.Error(w, "callback already received", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "invalid fixture callback", http.StatusBadRequest)
		return
	}
	_, _ = io.WriteString(w, "fixture authorization complete")
}

func (h *oauthElicitationHarness) clientFetch(ctx context.Context, redirect string, args *sdkauth.AuthorizationArgs, present func(context.Context, string) error) (*sdkauth.AuthorizationResult, error) {
	if redirect != h.callback.URL+"/oauth/callback" || args == nil || present == nil {
		return nil, errors.New("invalid fixture callback or presenter")
	}
	pending := make(chan elicitationCallbackResult, 1)
	h.mu.Lock()
	if h.pending != nil {
		h.mu.Unlock()
		return nil, errors.New("overlapping fixture authorization")
	}
	h.pending = pending
	h.mu.Unlock()
	h.clientUI.Add(1)
	defer func() {
		h.mu.Lock()
		if h.pending == pending {
			h.pending = nil
		}
		h.mu.Unlock()
	}()
	// The callback listener is already accepting requests when the UI sees URL.
	if err := present(ctx, args.URL); err != nil {
		return nil, err
	}
	select {
	case result := <-pending:
		return result.result, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *oauthElicitationHarness) directFetch(ctx context.Context, redirect string, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
	h.directUI.Add(1)
	return h.fixture.fetch(ctx, redirect, args)
}

type oauthElicitationPeer struct {
	stream       *oauthStdioClient
	modern       bool
	capabilities map[string]any
}

func (h *oauthElicitationHarness) connect(t *testing.T, modern bool, capabilities map[string]any) *oauthElicitationPeer {
	t.Helper()
	return connectOAuthElicitationWithOptions(t, h.root, modern, capabilities, ConnectOptions{
		ClientAuthorizationCodeFetcher: h.clientFetch,
		AuthorizationCodeFetcher:       h.directFetch,
	})
}

func connectOAuthElicitationWithOptions(t *testing.T, root string, modern bool, capabilities map[string]any, opts ConnectOptions) *oauthElicitationPeer {
	t.Helper()
	client, proxy := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	p := &oauthStdioClient{conn: client, proxy: proxy, reader: wire.NewReader(client), writer: wire.NewWriter(client),
		capture: &oauthStdioCapture{}, cancel: cancel, done: make(chan error, 1)}
	go func() {
		p.done <- ConnectWithOptions(ctx, root, "fixture", "cursor", proxy, io.MultiWriter(p.capture, proxy), opts)
	}()
	t.Cleanup(func() { p.close(t) })
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	return &oauthElicitationPeer{stream: p, modern: modern, capabilities: capabilities}
}

func (p *oauthElicitationPeer) send(t *testing.T, method, id string, params map[string]any) {
	t.Helper()
	copy := make(map[string]any, len(params)+1)
	for key, value := range params {
		copy[key] = value
	}
	if p.modern {
		copy["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion":    wire.ModernProtocolVersion,
			"io.modelcontextprotocol/clientCapabilities": p.capabilities,
			"io.modelcontextprotocol/clientInfo":         map[string]string{"name": "fixture-client", "version": "1"},
		}
	}
	p.stream.send(t, method, id, copy)
}

func (p *oauthElicitationPeer) frame(t *testing.T) wire.Message {
	t.Helper()
	_ = p.stream.conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	message, err := p.stream.reader.Read()
	if err != nil {
		t.Fatalf("read elicitation MCP frame: %v; output: %s", err, p.stream.capture.String())
	}
	if !bytes.Equal(message["jsonrpc"], wire.Raw("2.0")) {
		t.Fatalf("private control escaped to MCP stdio: %s", wire.Raw(message))
	}
	return message
}

func (p *oauthElicitationPeer) response(t *testing.T, id string) wire.Message {
	t.Helper()
	return p.responseIncludingError(t, id, false)
}

func (p *oauthElicitationPeer) responseIncludingError(t *testing.T, id string, allowError bool) wire.Message {
	t.Helper()
	for {
		message := p.frame(t)
		if message.Method() != "" {
			if message.HasID() {
				t.Fatalf("unexpected server request while waiting for %s: %s", id, wire.Raw(message))
			}
			switch message.Method() {
			case "notifications/tools/list_changed", "notifications/resources/list_changed", "notifications/prompts/list_changed", "notifications/elicitation/complete":
				continue
			default:
				t.Fatalf("unexpected notification: %s", wire.Raw(message))
			}
		}
		if !bytes.Equal(message.ID(), wire.Raw(id)) || (!allowError && message["error"] != nil) {
			t.Fatalf("unexpected response for %s: %s", id, wire.Raw(message))
		}
		return message
	}
}

func (p *oauthElicitationPeer) initialize(t *testing.T) {
	t.Helper()
	if p.modern {
		p.send(t, "server/discover", "discover", nil)
		p.response(t, "discover")
		return
	}
	p.send(t, "initialize", "initialize", map[string]any{
		"protocolVersion": "2025-11-25", "capabilities": p.capabilities,
		"clientInfo": map[string]string{"name": "fixture-client", "version": "1"},
	})
	p.response(t, "initialize")
	if err := p.stream.writer.Write(wire.Notification("notifications/initialized", nil)); err != nil {
		t.Fatal(err)
	}
}

func (p *oauthElicitationPeer) catalog(t *testing.T, id, expected string) {
	t.Helper()
	p.send(t, "tools/list", id, nil)
	response := p.response(t, id)
	var result struct {
		Tools []struct {
			Name        string
			InputSchema map[string]any
		}
	}
	if err := json.Unmarshal(response["result"], &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Tools) != 1 || result.Tools[0].Name != expected {
		t.Fatalf("unexpected tool catalog, want %s: %s", expected, response["result"])
	}
	if expected == elicitationAuthTool && (result.Tools[0].InputSchema["type"] != "object" || result.Tools[0].InputSchema["additionalProperties"] != false) {
		t.Fatalf("authentication tool does not have a bounded empty object schema: %s", response["result"])
	}
}

func elicitationToolResult(t *testing.T, response wire.Message, wantError bool) string {
	t.Helper()
	var result struct {
		IsError    bool
		ResultType string
		Content    []struct{ Text string }
	}
	if err := json.Unmarshal(response["result"], &result); err != nil {
		t.Fatal(err)
	}
	if result.IsError != wantError || result.ResultType == "input_required" {
		t.Fatalf("unexpected authentication tool outcome: %s", response["result"])
	}
	var text strings.Builder
	for _, content := range result.Content {
		text.WriteString(content.Text)
	}
	return text.String()
}

type oauthElicitationPrompt struct {
	URL           string
	ElicitationID string
	Mode          string
}

func (h *oauthElicitationHarness) prompt(t *testing.T, raw json.RawMessage, legacy bool) oauthElicitationPrompt {
	t.Helper()
	var prompt oauthElicitationPrompt
	if err := json.Unmarshal(raw, &prompt); err != nil {
		t.Fatal(err)
	}
	if prompt.Mode != "url" || (legacy && prompt.ElicitationID == "") {
		t.Fatalf("invalid URL elicitation parameters: %s", raw)
	}
	parsed, err := url.Parse(prompt.URL)
	if err != nil || parsed.Scheme+"://"+parsed.Host != h.fixture.http.URL || parsed.Path != "/authorize" {
		t.Fatalf("elicitation points outside the temporary provider: %s", raw)
	}
	if parsed.Query().Get("redirect_uri") != h.callback.URL+"/oauth/callback" || parsed.Query().Get("code_challenge") == "" || parsed.Query().Get("state") == "" {
		t.Fatalf("elicitation lost callback, PKCE or state: %s", raw)
	}
	return prompt
}

func (h *oauthElicitationHarness) open(t *testing.T, authorizationURL string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(request *http.Request, _ []*http.Request) error {
		if request.URL.Scheme+"://"+request.URL.Host != h.callback.URL || request.URL.Path != "/oauth/callback" {
			return errors.New("fixture redirect left the temporary callback")
		}
		return nil
	}}
	response, err := client.Get(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("fixture callback failed: HTTP %d", response.StatusCode)
	}
	h.openedURLs.Add(1)
}

func (h *oauthElicitationHarness) authenticate(t *testing.T, p *oauthElicitationPeer, action string) {
	t.Helper()
	h.authenticateWithOutcome(t, p, action, action != "accept")
}

func (h *oauthElicitationHarness) authenticateWithOutcome(t *testing.T, p *oauthElicitationPeer, action string, wantError bool) string {
	t.Helper()
	p.send(t, "tools/call", "authenticate", map[string]any{"name": elicitationAuthTool, "arguments": map[string]any{}})
	responseID := "authenticate"
	if p.modern {
		response := p.response(t, responseID)
		var result struct {
			ResultType    string
			RequestState  json.RawMessage
			InputRequests map[string]struct {
				Method string
				Params json.RawMessage
			}
		}
		if err := json.Unmarshal(response["result"], &result); err != nil {
			t.Fatal(err)
		}
		request, ok := result.InputRequests["oauth"]
		if result.ResultType != "input_required" || len(result.RequestState) == 0 || bytes.Equal(result.RequestState, []byte("null")) ||
			!ok || len(result.InputRequests) != 1 || request.Method != "elicitation/create" {
			t.Fatalf("modern authentication did not return InputRequiredResult: %s", response["result"])
		}
		prompt := h.prompt(t, request.Params, false)
		if action == "accept" {
			h.open(t, prompt.URL)
		}
		responseID = "authenticate-resume"
		p.send(t, "tools/call", responseID, map[string]any{
			"name": elicitationAuthTool, "arguments": map[string]any{},
			"requestState": result.RequestState, "inputResponses": map[string]any{"oauth": map[string]string{"action": action}},
		})
	} else {
		request := p.frame(t)
		if request.Method() != "elicitation/create" || !request.HasID() {
			t.Fatalf("legacy authentication did not send elicitation/create: %s", wire.Raw(request))
		}
		prompt := h.prompt(t, request["params"], true)
		if err := p.stream.writer.Write(wire.Result(request.ID(), map[string]string{"action": action})); err != nil {
			t.Fatal(err)
		}
		if action == "accept" {
			h.open(t, prompt.URL)
		}
	}
	return elicitationToolResult(t, p.response(t, responseID), wantError)
}

func assertElicitationCredentialsPrivate(t *testing.T, p *oauthElicitationPeer) {
	t.Helper()
	for _, secret := range []string{"private-stdio-code-", "private-stdio-access-", "private-stdio-refresh-", "client_secret"} {
		if strings.Contains(p.stream.capture.String(), secret) {
			t.Fatalf("OAuth credential escaped into MCP stdio: %s", secret)
		}
	}
}

func TestOAuthElicitationURLFlow(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, auto := range []bool{false, true} {
			t.Run(fmt.Sprintf("modern=%t/auto=%t", modern, auto), func(t *testing.T) {
				h := newOAuthElicitationHarness(t, &auto)
				p := h.connect(t, modern, map[string]any{"elicitation": map[string]any{"url": map[string]any{}}})
				p.initialize(t)
				if h.clientUI.Load() != 0 || h.directUI.Load() != 0 || h.fixture.exchanges.Load() != 0 {
					t.Fatal("initialization started an authentication UI")
				}
				p.catalog(t, "before-login", elicitationAuthTool)
				if h.clientUI.Load() != 0 || h.directUI.Load() != 0 {
					t.Fatal("tool discovery started an authentication UI")
				}
				h.authenticate(t, p, "accept")
				p.catalog(t, "after-login", "echo")
				p.send(t, "tools/call", "echo", map[string]any{"name": "echo", "arguments": map[string]any{}})
				elicitationToolResult(t, p.response(t, "echo"), false)
				if h.clientUI.Load() != 1 || h.directUI.Load() != 0 || h.openedURLs.Load() != 1 || h.fixture.exchanges.Load() != 1 || h.fixture.bearerToolCalls.Load() != 1 {
					t.Fatalf("URL flow lost priority or authenticated execution: client=%d direct=%d opened=%d exchanges=%d bearerTools=%d",
						h.clientUI.Load(), h.directUI.Load(), h.openedURLs.Load(), h.fixture.exchanges.Load(), h.fixture.bearerToolCalls.Load())
				}
				assertElicitationCredentialsPrivate(t, p)
			})
		}
	}
}

func TestOAuthElicitationMissingURLCapabilityUsesManualAuth(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, form := range []bool{false, true} {
			t.Run(fmt.Sprintf("modern=%t/form=%t", modern, form), func(t *testing.T) {
				// No settings file: this also checks the default is no automatic UI.
				h := newOAuthElicitationHarness(t, nil)
				capabilities := map[string]any{}
				if form {
					capabilities["elicitation"] = map[string]any{"form": map[string]any{}}
				}
				p := h.connect(t, modern, capabilities)
				p.initialize(t)
				p.catalog(t, "manual-catalog", elicitationAuthTool)
				p.send(t, "tools/call", "manual-auth", map[string]any{"name": elicitationAuthTool, "arguments": map[string]any{}})
				message := elicitationToolResult(t, p.response(t, "manual-auth"), true)
				if !strings.Contains(message, "agentcfg auth login") || !strings.Contains(message, "fixture") {
					t.Fatalf("manual login instruction is missing: %s", message)
				}
				if h.clientUI.Load() != 0 || h.directUI.Load() != 0 || h.fixture.exchanges.Load() != 0 {
					t.Fatal("a client without URL capability triggered a UI or login")
				}
				assertElicitationCredentialsPrivate(t, p)
			})
		}
	}
}

func TestOAuthElicitationBrowserFallbackRequiresOptIn(t *testing.T) {
	for _, auto := range []bool{false, true} {
		t.Run(fmt.Sprintf("auto=%t", auto), func(t *testing.T) {
			h := newOAuthElicitationHarness(t, &auto)
			p := h.connect(t, false, nil)
			p.initialize(t)
			want := elicitationAuthTool
			if auto {
				want = "echo"
			}
			p.catalog(t, "fallback-catalog", want)
			wantDirect := int32(0)
			if auto {
				wantDirect = 1
				p.send(t, "tools/call", "fallback-echo", map[string]any{"name": "echo", "arguments": map[string]any{}})
				elicitationToolResult(t, p.response(t, "fallback-echo"), false)
			}
			if h.directUI.Load() != wantDirect || h.clientUI.Load() != 0 || h.fixture.exchanges.Load() != wantDirect {
				t.Fatalf("browser fallback ignored opt-in: direct=%d want=%d client=%d exchanges=%d",
					h.directUI.Load(), wantDirect, h.clientUI.Load(), h.fixture.exchanges.Load())
			}
		})
	}
}

func TestOAuthElicitationDeclineAndCancelDoNotOpenFallback(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, action := range []string{"decline", "cancel"} {
			t.Run(fmt.Sprintf("modern=%t/%s", modern, action), func(t *testing.T) {
				auto := true
				h := newOAuthElicitationHarness(t, &auto)
				p := h.connect(t, modern, map[string]any{"elicitation": map[string]any{"url": map[string]any{}}})
				p.initialize(t)
				p.catalog(t, "before-refusal", elicitationAuthTool)
				h.authenticate(t, p, action)
				p.catalog(t, "after-refusal", elicitationAuthTool)
				if h.clientUI.Load() != 1 || h.directUI.Load() != 0 || h.fixture.exchanges.Load() != 0 || h.openedURLs.Load() != 0 {
					t.Fatalf("refusing URL consent opened a fallback: client=%d direct=%d exchanges=%d opened=%d",
						h.clientUI.Load(), h.directUI.Load(), h.fixture.exchanges.Load(), h.openedURLs.Load())
				}
				assertElicitationCredentialsPrivate(t, p)
			})
		}
	}
}

func TestOAuthElicitationReloadsGrantAfterManualLogin(t *testing.T) {
	h := newOAuthElicitationHarness(t, nil)
	p := h.connect(t, false, nil)
	p.initialize(t)
	p.catalog(t, "before-manual-login", elicitationAuthTool)
	p.send(t, "tools/call", "manual-instruction", map[string]any{"name": elicitationAuthTool, "arguments": map[string]any{}})
	elicitationToolResult(t, p.response(t, "manual-instruction"), true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Exercise the same login operation used by the CLI with a separate handler
	// and store instance, while the existing stdio connection stays attached.
	if err := auth.Login(ctx, h.server, auth.Options{Home: h.root,
		AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
			return h.fixture.fetch(ctx, h.server.OAuth.RedirectURL, args)
		},
	}); err != nil {
		t.Fatal(err)
	}
	p.catalog(t, "after-manual-login", "echo")
	p.send(t, "tools/call", "manual-echo", map[string]any{"name": "echo", "arguments": map[string]any{}})
	elicitationToolResult(t, p.response(t, "manual-echo"), false)
	p.stream.close(t)
	second := h.connect(t, false, nil)
	second.initialize(t)
	second.catalog(t, "reconnected-catalog", "echo")
	second.send(t, "tools/call", "reconnected-echo", map[string]any{"name": "echo", "arguments": map[string]any{}})
	elicitationToolResult(t, second.response(t, "reconnected-echo"), false)
	if h.clientUI.Load() != 0 || h.directUI.Load() != 0 || h.fixture.exchanges.Load() != 1 || h.fixture.bearerToolCalls.Load() != 2 {
		t.Fatalf("manual grant was not reused by current and new connections: client=%d direct=%d exchanges=%d bearerTools=%d",
			h.clientUI.Load(), h.directUI.Load(), h.fixture.exchanges.Load(), h.fixture.bearerToolCalls.Load())
	}
	assertElicitationCredentialsPrivate(t, second)
}

func TestOAuthElicitationSharedLoginUsesRequestingClient(t *testing.T) {
	for _, olderURL := range []bool{false, true} {
		t.Run(fmt.Sprintf("older-client-url=%t", olderURL), func(t *testing.T) {
			h := newOAuthElicitationHarness(t, nil, oauthElicitationFixtureOptions{sharing: "shared"})
			olderCapabilities := map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}
			if olderURL {
				olderCapabilities = map[string]any{"elicitation": map[string]any{"url": map[string]any{}}}
			}
			older := h.connect(t, false, olderCapabilities)
			older.initialize(t)
			older.catalog(t, "older-before-login", elicitationAuthTool)
			requesting := h.connect(t, false, map[string]any{"elicitation": map[string]any{"url": map[string]any{}}})
			requesting.initialize(t)
			requesting.catalog(t, "requesting-before-login", elicitationAuthTool)
			h.authenticate(t, requesting, "accept")
			requesting.catalog(t, "requesting-after-login", "echo")
			// This response also proves the idle client did not receive a queued
			// unsolicited URL request while the other client was authenticating.
			older.catalog(t, "older-after-login", "echo")
			for id, peer := range map[string]*oauthElicitationPeer{"older-echo": older, "requesting-echo": requesting} {
				peer.send(t, "tools/call", id, map[string]any{"name": "echo", "arguments": map[string]any{}})
				elicitationToolResult(t, peer.response(t, id), false)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			status, err := GetStatus(ctx, h.root)
			if err != nil || len(status.Backends) != 1 || status.Backends[0].Clients != 2 {
				t.Fatalf("clients did not share the authenticated backend: %v %+v", err, status)
			}
			if h.clientUI.Load() != 1 || h.directUI.Load() != 0 || h.fixture.exchanges.Load() != 1 || h.fixture.bearerToolCalls.Load() != 2 {
				t.Fatalf("shared login was not routed once to the requesting client: client=%d direct=%d exchanges=%d tools=%d",
					h.clientUI.Load(), h.directUI.Load(), h.fixture.exchanges.Load(), h.fixture.bearerToolCalls.Load())
			}
		})
	}
}

func TestOAuthElicitationPublicLegacyProviderDoesNotAdvertiseModern(t *testing.T) {
	h := newOAuthElicitationHarness(t, nil, oauthElicitationFixtureOptions{legacyOnly: true, public: true})
	modern := h.connect(t, true, nil)
	modern.send(t, "server/discover", "legacy-provider-discover", nil)
	response := modern.responseIncludingError(t, "legacy-provider-discover", true)
	if response["error"] == nil {
		var discovery struct{ SupportedVersions []string }
		if err := json.Unmarshal(response["result"], &discovery); err != nil {
			t.Fatal(err)
		}
		for _, version := range discovery.SupportedVersions {
			if version == wire.ModernProtocolVersion {
				t.Fatal("facade advertised modern support for a known legacy-only provider")
			}
		}
	}
	modern.stream.close(t)
	legacy := h.connect(t, false, nil)
	legacy.initialize(t)
	legacy.catalog(t, "legacy-public-catalog", "echo")
	legacy.send(t, "tools/call", "legacy-public-echo", map[string]any{"name": "echo", "arguments": map[string]any{}})
	elicitationToolResult(t, legacy.response(t, "legacy-public-echo"), false)
	if h.clientUI.Load() != 0 || h.directUI.Load() != 0 || h.fixture.exchanges.Load() != 0 {
		t.Fatal("public legacy negotiation triggered authentication")
	}
}

func TestOAuthElicitationLegacyProviderRetainsGrantAcrossNegotiation(t *testing.T) {
	h := newOAuthElicitationHarness(t, nil, oauthElicitationFixtureOptions{legacyOnly: true})
	modern := h.connect(t, true, map[string]any{"elicitation": map[string]any{"url": map[string]any{}}})
	modern.initialize(t)
	modern.catalog(t, "legacy-protected-catalog", elicitationAuthTool)
	message := h.authenticateWithOutcome(t, modern, "accept", true)
	if !strings.Contains(strings.ToLower(message), "reconnect") {
		t.Fatalf("legacy-only provider did not request protocol renegotiation: %s", message)
	}
	if h.fixture.exchanges.Load() != 1 {
		t.Fatal("consent did not persist a grant before protocol renegotiation")
	}
	modern.stream.close(t)
	legacy := h.connect(t, false, nil)
	legacy.initialize(t)
	legacy.catalog(t, "renegotiated-catalog", "echo")
	legacy.send(t, "tools/call", "renegotiated-echo", map[string]any{"name": "echo", "arguments": map[string]any{}})
	elicitationToolResult(t, legacy.response(t, "renegotiated-echo"), false)
	if h.clientUI.Load() != 1 || h.directUI.Load() != 0 || h.fixture.exchanges.Load() != 1 || h.fixture.bearerToolCalls.Load() != 1 {
		t.Fatalf("protocol renegotiation lost the saved grant: client=%d direct=%d exchanges=%d tools=%d",
			h.clientUI.Load(), h.directUI.Load(), h.fixture.exchanges.Load(), h.fixture.bearerToolCalls.Load())
	}
	assertElicitationCredentialsPrivate(t, legacy)
}

func TestOAuthElicitationURLOnlyConfigurationRemainsUnchanged(t *testing.T) {
	fixture := newOAuthStdioFixture(t)
	root := oauthStdioHome(t, fixture.http.URL+"/mcp", "isolated", nil)
	configPath := filepath.Join(root, ".agentcfg", "mcp.json")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	serveOAuthStdioBroker(t, root)
	var clientUI, directUI atomic.Int32
	p := connectOAuthElicitationWithOptions(t, root, false,
		map[string]any{"elicitation": map[string]any{"url": map[string]any{}}},
		ConnectOptions{
			ClientAuthorizationCodeFetcher: func(ctx context.Context, redirect string, args *sdkauth.AuthorizationArgs, present func(context.Context, string) error) (*sdkauth.AuthorizationResult, error) {
				clientUI.Add(1)
				if err := present(ctx, args.URL); err != nil {
					return nil, err
				}
				// The URL-only policy chooses its own redirect. Reuse the existing
				// provider adapter to return that complete callback; the preceding
				// tests separately exercise a real bound callback listener.
				return fixture.fetch(ctx, redirect, args)
			},
			AuthorizationCodeFetcher: func(ctx context.Context, redirect string, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
				directUI.Add(1)
				return fixture.fetch(ctx, redirect, args)
			},
		})
	p.initialize(t)
	p.catalog(t, "implicit-catalog", elicitationAuthTool)
	p.send(t, "tools/call", "implicit-authenticate", map[string]any{"name": elicitationAuthTool, "arguments": map[string]any{}})
	request := p.frame(t)
	if request.Method() != "elicitation/create" || !request.HasID() {
		t.Fatalf("URL-only configuration did not offer client URL authorization: %s", wire.Raw(request))
	}
	var prompt oauthElicitationPrompt
	if err := json.Unmarshal(request["params"], &prompt); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(prompt.URL)
	if err != nil || prompt.Mode != "url" || target.Scheme+"://"+target.Host != fixture.http.URL || target.Path != "/authorize" {
		t.Fatalf("invalid implicit authorization request: %s", request["params"])
	}
	if err := p.stream.writer.Write(wire.Result(request.ID(), map[string]string{"action": "accept"})); err != nil {
		t.Fatal(err)
	}
	elicitationToolResult(t, p.response(t, "implicit-authenticate"), false)
	p.catalog(t, "implicit-authenticated-catalog", "echo")
	p.send(t, "tools/call", "implicit-echo", map[string]any{"name": "echo", "arguments": map[string]any{}})
	elicitationToolResult(t, p.response(t, "implicit-echo"), false)
	if clientUI.Load() != 1 || directUI.Load() != 0 || fixture.exchanges.Load() != 1 || fixture.bearerToolCalls.Load() != 1 {
		t.Fatalf("implicit client authorization did not use the stored grant: client=%d direct=%d exchanges=%d tools=%d",
			clientUI.Load(), directUI.Load(), fixture.exchanges.Load(), fixture.bearerToolCalls.Load())
	}
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("URL elicitation rewrote the canonical temporary MCP configuration")
	}
	assertElicitationCredentialsPrivate(t, p)
}

func TestOAuthElicitationDoesNotReplayAppliedToolAfterLostResponse(t *testing.T) {
	h := newOAuthElicitationHarness(t, nil)
	p := h.connect(t, false, map[string]any{"elicitation": map[string]any{"url": map[string]any{}}})
	p.initialize(t)
	p.catalog(t, "before-lost-response-login", elicitationAuthTool)
	h.authenticate(t, p, "accept")
	p.catalog(t, "before-lost-response-tool", "echo")
	// The fixture applies/counts the tool before dropping the HTTP connection,
	// so replaying after the lost response would duplicate a completed effect.
	h.fixture.dropTool.Store(true)
	p.send(t, "tools/call", "lost-response-tool", map[string]any{"name": "echo", "arguments": map[string]any{}})
	response := p.responseIncludingError(t, "lost-response-tool", true)
	var diagnostic string
	if response["error"] != nil {
		var failure struct{ Message string }
		if err := json.Unmarshal(response["error"], &failure); err != nil {
			t.Fatal(err)
		}
		diagnostic = failure.Message
	} else {
		diagnostic = elicitationToolResult(t, response, true)
	}
	if !strings.Contains(strings.ToLower(diagnostic), "not retried") {
		t.Fatalf("lost response did not explain the uncertain operation: %s", diagnostic)
	}
	if h.fixture.toolCalls.Load() != 1 || h.fixture.bearerToolCalls.Load() != 1 ||
		h.clientUI.Load() != 1 || h.directUI.Load() != 0 || h.fixture.exchanges.Load() != 1 {
		t.Fatalf("lost tool response was replayed or restarted OAuth: tools=%d bearerTools=%d client=%d direct=%d exchanges=%d",
			h.fixture.toolCalls.Load(), h.fixture.bearerToolCalls.Load(), h.clientUI.Load(), h.directUI.Load(), h.fixture.exchanges.Load())
	}
	assertElicitationCredentialsPrivate(t, p)
}

func TestOAuthElicitationOlderLegacyVersionAfterConsentRequiresReconnect(t *testing.T) {
	h := newOAuthElicitationHarness(t, nil, oauthElicitationFixtureOptions{legacyOnly: true, legacyVersion: "2025-03-26"})
	initialize := func(p *oauthElicitationPeer) string {
		t.Helper()
		p.send(t, "initialize", "version-initialize", map[string]any{
			"protocolVersion": "2025-11-25", "capabilities": p.capabilities,
			"clientInfo": map[string]string{"name": "fixture-client", "version": "1"},
		})
		response := p.response(t, "version-initialize")
		var result struct{ ProtocolVersion string }
		if err := json.Unmarshal(response["result"], &result); err != nil {
			t.Fatal(err)
		}
		if err := p.stream.writer.Write(wire.Notification("notifications/initialized", nil)); err != nil {
			t.Fatal(err)
		}
		return result.ProtocolVersion
	}
	first := h.connect(t, false, map[string]any{"elicitation": map[string]any{"url": map[string]any{}}})
	if version := initialize(first); version != "2025-11-25" {
		t.Fatalf("blocked initialization did not bootstrap URL elicitation: %s", version)
	}
	first.catalog(t, "before-legacy-version-consent", elicitationAuthTool)
	message := h.authenticateWithOutcome(t, first, "accept", true)
	if !strings.Contains(strings.ToLower(message), "reconnect") {
		t.Fatalf("changed legacy version did not require renegotiation: %s", message)
	}
	if h.fixture.exchanges.Load() != 1 || h.fixture.toolCalls.Load() != 0 {
		t.Fatalf("protocol mismatch lost its grant or executed a tool: exchanges=%d tools=%d", h.fixture.exchanges.Load(), h.fixture.toolCalls.Load())
	}
	first.stream.close(t)
	second := h.connect(t, false, nil)
	if version := initialize(second); version != "2025-03-26" {
		t.Fatalf("reconnected initialization did not preserve the provider's version: %s", version)
	}
	second.catalog(t, "after-legacy-version-reconnect", "echo")
	second.send(t, "tools/call", "old-version-echo", map[string]any{"name": "echo", "arguments": map[string]any{}})
	elicitationToolResult(t, second.response(t, "old-version-echo"), false)
	if h.clientUI.Load() != 1 || h.directUI.Load() != 0 || h.fixture.exchanges.Load() != 1 || h.fixture.bearerToolCalls.Load() != 1 {
		t.Fatalf("legacy version renegotiation did not reuse its grant: client=%d direct=%d exchanges=%d tools=%d",
			h.clientUI.Load(), h.directUI.Load(), h.fixture.exchanges.Load(), h.fixture.bearerToolCalls.Load())
	}
	assertElicitationCredentialsPrivate(t, second)
}
