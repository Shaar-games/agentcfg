package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/clientauth"
	"agentcfg/internal/config"
)

func proxyTestStore(t *testing.T) auth.Store {
	t.Helper()
	store, err := auth.NewEncryptedFileStore(t.TempDir(), bytes.Repeat([]byte{0x47}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func proxyTestIdentity() (clientauth.Resource, clientauth.Principal) {
	resource := clientauth.Resource{URL: "http://127.0.0.1:39001/mcp/fixture", Name: "fixture", Identity: "temporary-fixture"}
	return resource, clientauth.Principal{ClientID: "client-a", GrantID: "grant-a", Resource: resource.URL}
}

func TestProxyPreservesHTTPAndSeparatesCredentialsAndSessions(t *testing.T) {
	var calls atomic.Int32
	const payload = `{"jsonrpc":"2.0","id":"native","method":"tools/call","params":{"name":"echo","arguments":{"value":9007199254740993},"_meta":{"custom":{"kept":true}}}}`
	const result = `{"jsonrpc":"2.0","id":"native","result":{"content":[],"extension":{"kept":9007199254740993}}}`
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer configured-upstream-secret" || r.Header.Get("X-Resource-Key") != "canonical-key" {
			t.Errorf("canonical credentials were lost: %v", r.Header)
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Local-Secret") != "" || r.Header.Get("Origin") != "" || r.Header.Get("Mcp-Hop") != "" {
			t.Error("local or hop headers reached upstream")
		}
		if n == 1 {
			body, _ := io.ReadAll(r.Body)
			if string(body) != payload || r.Header.Get("Mcp-Param-Tenant") != "tenant-1" || r.Header.Get("Mcp-Protocol-Version") != "2026-07-28" {
				t.Error("MCP body or metadata changed")
			}
			w.Header().Set("Mcp-Session-Id", "upstream-private-session")
			w.Header().Set("Set-Cookie", "upstream=secret")
			w.Header().Set("Authorization", "Bearer response-secret")
			w.Header().Set("X-Upstream-Secret", "hidden")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, result)
			return
		}
		if r.Header.Get("Mcp-Session-Id") != "upstream-private-session" {
			t.Error("session mapping did not reach upstream")
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Custom", "retained")
		_, _ = io.WriteString(w, "event: message\ndata: {\"method\":\"notifications/progress\"}\n\n")
	}))
	defer remote.Close()
	server := config.Server{Name: "fixture", Type: "streamable-http", URL: remote.URL, Headers: map[string]string{"Authorization": "Bearer configured-upstream-secret", "X-Resource-Key": "canonical-key"}}
	resource, principal := proxyTestIdentity()
	p := NewProxy(ProxyOptions{Store: proxyTestStore(t)})
	t.Cleanup(func() { _ = p.Close() })
	request := httptest.NewRequest(http.MethodPost, resource.URL, strings.NewReader(payload))
	request.Header = http.Header{"Authorization": {"Bearer local-client-token"}, "Cookie": {"client=private"}, "Origin": {"https://client.invalid"}, "X-Local-Secret": {"private"}, "Mcp-Param-Tenant": {"tenant-1"}, "Mcp-Protocol-Version": {"2026-07-28"}, "Connection": {"Mcp-Hop"}, "Mcp-Hop": {"discard"}}
	response := httptest.NewRecorder()
	p.ServeMCP(response, request, resource, principal, server)
	if response.Code != http.StatusOK || response.Body.String() != result {
		t.Fatalf("unexpected proxy result: %d %s", response.Code, response.Body.String())
	}
	localSession := response.Header().Get("Mcp-Session-Id")
	if localSession == "" || localSession == "upstream-private-session" {
		t.Fatal("upstream session identifier was exposed")
	}
	for _, key := range []string{"Set-Cookie", "Authorization", "X-Upstream-Secret"} {
		if response.Header().Get(key) != "" {
			t.Errorf("upstream secret header %s escaped", key)
		}
	}
	get := func(principal clientauth.Principal, resource clientauth.Resource, method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, resource.URL, nil)
		r.Header.Set("Mcp-Session-Id", localSession)
		w := httptest.NewRecorder()
		p.ServeMCP(w, r, resource, principal, server)
		return w
	}
	other := principal
	other.GrantID = "grant-other"
	if got := get(other, resource, http.MethodGet); got.Code != http.StatusNotFound {
		t.Fatalf("another grant reused a session: %d", got.Code)
	}
	otherResource := resource
	otherResource.Identity = "other-resource"
	if got := get(principal, otherResource, http.MethodGet); got.Code != http.StatusNotFound {
		t.Fatal("another resource reused a session")
	}
	if calls.Load() != 1 {
		t.Fatal("rejected sessions reached upstream")
	}
	if got := get(principal, resource, http.MethodGet); got.Code != http.StatusOK || got.Header().Get("Mcp-Custom") != "retained" || !strings.Contains(got.Body.String(), "notifications/progress") {
		t.Fatal("SSE or MCP response headers changed")
	}
	if got := get(principal, resource, http.MethodDelete); got.Code != http.StatusNoContent {
		t.Fatal("DELETE was not forwarded")
	}
	if got := get(principal, resource, http.MethodGet); got.Code != http.StatusNotFound {
		t.Fatal("deleted local session remained available")
	}
}

func TestProxyDoesNotReplayOrFollowRedirect(t *testing.T) {
	for _, mode := range []string{"lost-response", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			var applied, redirected atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
			defer target.Close()
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				applied.Add(1)
				if mode == "redirect" {
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
					return
				}
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close()
			}))
			defer remote.Close()
			resource, principal := proxyTestIdentity()
			server := config.Server{Name: "fixture", Type: "streamable-http", URL: remote.URL, Headers: map[string]string{"Authorization": "Bearer private-upstream"}}
			p := NewProxy(ProxyOptions{Store: proxyTestStore(t)})
			defer p.Close()
			response := httptest.NewRecorder()
			p.ServeMCP(response, httptest.NewRequest(http.MethodPost, resource.URL, strings.NewReader(`{"method":"tools/call"}`)), resource, principal, server)
			if response.Code != http.StatusBadGateway || applied.Load() != 1 || redirected.Load() != 0 {
				t.Fatalf("operation replayed or redirect followed: status=%d calls=%d redirects=%d", response.Code, applied.Load(), redirected.Load())
			}
			if strings.Contains(response.Body.String(), remote.URL) || strings.Contains(response.Body.String(), "private-upstream") {
				t.Fatal("diagnostic exposed credentials or URL")
			}
		})
	}
}

func TestProxyChallengesHTTPOnlyAndKeepsResourcesSeparate(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		status                  int
		header                  string
		challenge, insufficient bool
	}{
		{"unauthorized", 401, `Bearer resource_metadata="https://provider.invalid/private"`, true, false},
		{"scope", 403, `Bearer error="insufficient_scope", scope="write"`, true, true},
		{"denied", 403, `Bearer error="access_denied"`, false, false},
		{"rpc-error", 200, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("local bearer reached the upstream")
				}
				w.Header().Set("WWW-Authenticate", tc.header)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"OAuth login required"}}`)
			}))
			defer remote.Close()
			resource, principal := proxyTestIdentity()
			var challenged bool
			p := NewProxy(ProxyOptions{Store: proxyTestStore(t), Challenge: func(w http.ResponseWriter, r *http.Request, got clientauth.Resource, owner clientauth.Principal, insufficient bool) {
				challenged = true
				if got != resource || owner != principal || insufficient != tc.insufficient {
					t.Error("challenge classification or owner changed")
				}
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="local-metadata"`)
				w.WriteHeader(401)
			}})
			defer p.Close()
			request := httptest.NewRequest(http.MethodPost, resource.URL, strings.NewReader(`{"method":"tools/call"}`))
			request.Header.Set("Authorization", "Bearer local-secret")
			response := httptest.NewRecorder()
			p.ServeMCP(response, request, resource, principal, config.Server{Name: "fixture", Type: "streamable-http", URL: remote.URL})
			if challenged != tc.challenge {
				t.Fatalf("HTTP challenge classification is wrong: %v", challenged)
			}
			if strings.Contains(response.Header().Get("WWW-Authenticate"), "provider.invalid") {
				t.Fatal("upstream OAuth challenge escaped")
			}
			_, saved := p.savedChallenge(resource)
			if saved != tc.challenge {
				t.Fatal("upstream challenge was not kept for its canonical resource")
			}
			other := resource
			other.Identity = "different-canonical-resource"
			if _, ok := p.savedChallenge(other); ok {
				t.Fatal("another resource acquired the upstream challenge")
			}
		})
	}
}

func TestProxyStreamingCancellationAndClose(t *testing.T) {
	for _, closeProxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "client-cancel", true: "proxy-close"}[closeProxy], func(t *testing.T) {
			upstreamStopped := make(chan struct{})
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"progress\":1}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(upstreamStopped)
			}))
			defer remote.Close()
			resource, principal := proxyTestIdentity()
			server := config.Server{Name: "fixture", Type: "streamable-http", URL: remote.URL, Headers: map[string]string{"Authorization": "Bearer canonical"}}
			p := NewProxy(ProxyOptions{Store: proxyTestStore(t)})
			defer p.Close()
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.ServeMCP(w, r, resource, principal, server) }))
			defer local.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, local.URL, nil)
			response, err := local.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			buffer := make([]byte, 6)
			if _, err := io.ReadFull(response.Body, buffer); err != nil {
				t.Fatal(err)
			}
			if string(buffer) != "data: " {
				t.Fatal("SSE was buffered or changed")
			}
			if closeProxy {
				_ = p.Close()
			} else {
				cancel()
			}
			select {
			case <-upstreamStopped:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream stream survived cancellation")
			}
		})
	}
}

func TestProxyPreservesCompressedHTTPBodies(t *testing.T) {
	var compressed bytes.Buffer
	zip := gzip.NewWriter(&compressed)
	_, _ = zip.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"custom":"kept"}}`))
	_ = zip.Close()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, compressed.Bytes()) || r.Header.Get("Content-Encoding") != "gzip" || r.Header.Get("Accept-Encoding") != "gzip" {
			t.Error("compressed request was altered")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	defer remote.Close()
	resource, principal := proxyTestIdentity()
	server := config.Server{Name: "fixture", Type: "streamable-http", URL: remote.URL, Headers: map[string]string{"Authorization": "Bearer upstream"}}
	p := NewProxy(ProxyOptions{Store: proxyTestStore(t)})
	defer p.Close()
	request := httptest.NewRequest(http.MethodPost, resource.URL, bytes.NewReader(compressed.Bytes()))
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	p.ServeMCP(response, request, resource, principal, server)
	if response.Code != 200 || response.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(response.Body.Bytes(), compressed.Bytes()) {
		t.Fatal("compressed response was altered")
	}
}

func TestProxyRedactsHTTPDiagnosticsButKeepsCorrelatedMCPError(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		keep       bool
	}{
		{"http-failure", "upstream access_token=private-token at https://issuer.invalid/private", 500, false},
		{"modern-method-error", `{"jsonrpc":"2.0","id":"probe","error":{"code":-32601,"message":"Unsupported discovery","data":{"versions":["2025-11-25"]}}}`, 404, true},
		{"unrelated-mcp-error", `{"jsonrpc":"2.0","id":"other","error":{"code":-32000,"message":"private-token"}}`, 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer remote.Close()
			resource, principal := proxyTestIdentity()
			p := NewProxy(ProxyOptions{Store: proxyTestStore(t)})
			defer p.Close()
			request := httptest.NewRequest(http.MethodPost, resource.URL, strings.NewReader(`{"jsonrpc":"2.0","id":"probe","method":"server/discover"}`))
			response := httptest.NewRecorder()
			p.ServeMCP(response, request, resource, principal, config.Server{Name: "fixture", Type: "streamable-http", URL: remote.URL, Headers: map[string]string{"Authorization": "Bearer upstream"}})
			if response.Code != tc.status {
				t.Fatalf("HTTP failure status changed: %d", response.Code)
			}
			if tc.keep && response.Body.String() != tc.body {
				t.Fatal("correlated MCP error was changed")
			}
			if !tc.keep && (response.Body.String() == tc.body || strings.Contains(response.Body.String(), "private-token")) {
				t.Fatal("upstream diagnostics escaped")
			}
		})
	}
}
