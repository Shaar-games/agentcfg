package gateway

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/ipc"
)

func gatewayTestRoot(t *testing.T, address string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "cache"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("AGENTCFG_OAUTH_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{57}, 32)))
	if err := os.Mkdir(filepath.Join(root, ".agentcfg"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeGatewayFixture(t, root, "agentcfg.json", map[string]any{"harnesses": []string{"cursor"}, "gateway": map[string]any{"enabled": true, "listen": address}})
	writeGatewayFixture(t, root, "mcp.json", map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{"type": "streamable-http", "url": "https://fixture.invalid/mcp", "harnesses": []string{"cursor"}}}})
	return root
}

func writeGatewayFixture(t *testing.T, root, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".agentcfg", name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayResourceBoundaryAndChallenges(t *testing.T) {
	root := gatewayTestRoot(t, "127.0.0.1:43877")
	h, err := New(t.Context(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Settings.Gateway.RouteKey = h.routeKey
	resource := config.GatewayResourceURL(cfg.Settings.Gateway, "fixture", "cursor")
	metadata := h.BaseURL() + "/.well-known/oauth-protected-resource" + strings.TrimPrefix(resource, h.BaseURL())
	for _, tt := range []struct {
		name, method, target, host, origin, bearer string
		status                                     int
	}{
		{name: "challenge", method: "POST", target: resource, status: 401},
		{name: "invalid bearer", method: "POST", target: resource, bearer: "Bearer upstream-secret", status: 401},
		{name: "metadata", method: "GET", target: metadata, status: 200},
		{name: "query rejected", method: "POST", target: resource + "?secret=no", status: 400},
		{name: "method rejected", method: "PUT", target: resource, status: 405},
		{name: "wrong host", method: "POST", target: resource, host: "attacker.invalid:43877", status: 400},
		{name: "foreign origin", method: "POST", target: resource, origin: "https://attacker.invalid", status: 403},
		{name: "null origin", method: "POST", target: resource, origin: "null", status: 403},
		{name: "other harness", method: "POST", target: config.GatewayResourceURL(cfg.Settings.Gateway, "fixture", "codex"), status: 404},
		{name: "unknown resource", method: "POST", target: config.GatewayResourceURL(cfg.Settings.Gateway, "missing", "cursor"), status: 404},
		{name: "noncanonical route", method: "POST", target: resource + "/", status: 404},
		{name: "unknown local capability", method: "POST", target: strings.Replace(resource, strings.Split(strings.TrimPrefix(resource, h.BaseURL()), "/")[2], strings.Repeat("A", 43), 1), status: 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.target, strings.NewReader(`{}`))
			if tt.host != "" {
				r.Host = tt.host
			}
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.bearer != "" {
				r.Header.Set("Authorization", tt.bearer)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tt.status == 401 && !strings.Contains(w.Header().Get("WWW-Authenticate"), metadata) {
				t.Fatalf("challenge did not identify local metadata: %s", w.Header())
			}
			if strings.Contains(w.Body.String(), "upstream-secret") || strings.Contains(w.Body.String(), "fixture.invalid") {
				t.Fatal("local response disclosed upstream credentials or URL")
			}
		})
	}
	before, _, err := h.resolve(resource)
	if err != nil {
		t.Fatal(err)
	}
	writeGatewayFixture(t, root, "mcp.json", map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{"type": "streamable-http", "url": "https://changed.invalid/mcp"}}})
	after, _, err := h.resolve(resource)
	if err != nil || before.Identity == after.Identity {
		t.Fatal("configuration change retained the old grant identity")
	}
	writeGatewayFixture(t, root, "agentcfg.json", map[string]any{"gateway": map[string]any{"enabled": false}})
	if _, _, err := h.resolve(resource); err == nil {
		t.Fatal("disabled gateway still resolved a protected resource")
	}
}

func TestGatewayBearerHeaderBoundary(t *testing.T) {
	for _, value := range []string{"Bearer local-token", "bearer    local-token"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", value)
		if token, ok := gatewayBearer(r); !ok || token != "local-token" {
			t.Fatalf("valid Bearer credentials rejected: %q", value)
		}
	}
	for _, values := range [][]string{{"Basic token"}, {"Bearer"}, {"Bearer token other"}, {"Bearer\ttoken"}, {"Bearer \ttoken"}, {"Bearer token", "Bearer another"}} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header["Authorization"] = values
		if _, ok := gatewayBearer(r); ok {
			t.Fatalf("ambiguous credentials accepted: %q", values)
		}
	}
}

func TestGatewayHarnessCanRetainStdioFallback(t *testing.T) {
	root := gatewayTestRoot(t, "127.0.0.1:43877")
	h, err := New(t.Context(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	settings := config.GatewaySettings{Enabled: true, Listen: "127.0.0.1:43877", RouteKey: h.routeKey}
	resource := config.GatewayResourceURL(settings, "fixture", "cursor")
	if _, _, err := h.resolve(resource); err != nil {
		t.Fatal(err)
	}
	writeGatewayFixture(t, root, "agentcfg.json", map[string]any{"harnesses": []string{"cursor", "codex"}, "gateway": map[string]any{"enabled": true, "listen": "127.0.0.1:43877", "harnesses": []string{"codex"}}})
	if _, _, err := h.resolve(resource); err == nil {
		t.Fatal("a client selected for stdio retained an active native HTTP endpoint")
	}
}

func TestGatewayRejectsRecursiveUpstreamBeforeNetwork(t *testing.T) {
	root := gatewayTestRoot(t, "127.0.0.1:43877")
	h, err := New(t.Context(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	settings := config.GatewaySettings{Enabled: true, Listen: "127.0.0.1:43877", RouteKey: h.routeKey}
	resource := config.GatewayResourceURL(settings, "fixture", "cursor")
	for _, upstream := range []string{resource, "http://localhost:43877/mcp/x/-", "http://localhost.:43877/mcp/x/-", "http://[::1]:43877/mcp/x/-", "http://127.0.0.2:43877/mcp/x/-", "https://127.0.0.1:43877/mcp/x/-"} {
		writeGatewayFixture(t, root, "mcp.json", map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{"type": "streamable-http", "url": upstream}}})
		if _, _, err := h.resolve(resource); err == nil || !strings.Contains(err.Error(), "itself") {
			t.Fatalf("recursive upstream was accepted: %s (%v)", upstream, err)
		}
	}
	writeGatewayFixture(t, root, "mcp.json", map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{"type": "streamable-http", "url": "http://127.0.0.1:43878/mcp"}}})
	if _, _, err := h.resolve(resource); err != nil {
		t.Fatalf("unrelated local MCP rejected: %v", err)
	}
}

func TestGatewayRuntimePrivateControlAndPortRelease(t *testing.T) {
	u, _ := url.Parse(unusedNativeCallback(t))
	root := gatewayTestRoot(t, u.Host)
	endpoint, err := ipc.ForService(root, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(endpoint.Directory); !os.IsNotExist(err) {
		t.Fatal("test IPC directory was already present")
	}
	t.Cleanup(func() { _ = os.RemoveAll(endpoint.Directory) })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if status, err := GetStatus(ctx, root); err != nil || status.Running {
		t.Fatalf("fresh status=%+v err=%v", status, err)
	}
	if _, err := os.Lstat(endpoint.Directory); !os.IsNotExist(err) {
		t.Fatal("read-only status created runtime files")
	}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, root, io.Discard) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			t.Error("gateway did not shut down")
		}
	})
	var status Status
	for {
		status, err = GetStatus(ctx, root)
		if err == nil && status.Running {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("gateway did not start")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if status.PID != os.Getpid() || status.URL != "http://"+u.Host {
		t.Fatalf("invalid live status: %+v", status)
	}
	// A second instance may not remove the first instance's IPC endpoint or port.
	if err := Run(ctx, root, io.Discard); err == nil {
		t.Fatal("duplicate service was accepted")
	}
	response, err := (&http.Client{Timeout: 2 * time.Second}).Get(status.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("live metadata status=%d", response.StatusCode)
	}
	brokerEndpoint, err := ipc.ForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if connection, err := ipc.Dial(ctx, brokerEndpoint); err == nil {
		connection.Close()
		t.Fatal("gateway listened on the broker endpoint")
	}
	if err := Stop(ctx, root); err != nil {
		t.Fatal(err)
	}
	if status, err := GetStatus(ctx, root); err != nil || status.Running {
		t.Fatalf("stopped status=%+v err=%v", status, err)
	}
	listener, err := net.Listen("tcp", u.Host)
	if err != nil {
		t.Fatalf("stop returned before releasing the HTTP port: %v", err)
	}
	_ = listener.Close()
}

func TestGatewayRuntimeReportsOccupiedPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	root := gatewayTestRoot(t, listener.Addr().String())
	endpoint, err := ipc.ForService(root, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(endpoint.Directory); !os.IsNotExist(err) {
		t.Fatal("test IPC directory was already present")
	}
	t.Cleanup(func() { _ = os.RemoveAll(endpoint.Directory) })
	if err := Run(t.Context(), root, io.Discard); err == nil || !strings.Contains(err.Error(), "listen on configured local gateway address") {
		t.Fatalf("occupied port failure was hidden: %v", err)
	}
	if status, err := GetStatus(t.Context(), root); err != nil || status.Running {
		t.Fatalf("failed startup published status=%+v (%v)", status, err)
	}
}

// Keep the explicit-store constructor usable without ambient credential keys.
func TestGatewayExplicitStoreDoesNotUseAmbientKey(t *testing.T) {
	root := gatewayTestRoot(t, "127.0.0.1:43877")
	t.Setenv("AGENTCFG_OAUTH_KEY", "invalid-ambient-key")
	store, err := auth.NewEncryptedFileStore(root, bytes.Repeat([]byte{19}, 32))
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(t.Context(), root, Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayRuntimeTLS(t *testing.T) {
	seed := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := seed.TLS.Certificates[0]
	client := seed.Client()
	client.Timeout = 3 * time.Second
	seed.Close()
	encodedKey, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(unusedNativeCallback(t))
	root := gatewayTestRoot(t, u.Host)
	for name, block := range map[string]*pem.Block{
		"gateway-cert.pem": {Type: "CERTIFICATE", Bytes: certificate.Certificate[0]},
		"gateway-key.pem":  {Type: "PRIVATE KEY", Bytes: encodedKey},
	} {
		if err := os.WriteFile(filepath.Join(root, name), pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeGatewayFixture(t, root, "agentcfg.json", map[string]any{"gateway": map[string]any{"enabled": true, "listen": u.Host, "tlsCert": "gateway-cert.pem", "tlsKey": "gateway-key.pem"}})
	endpoint, err := ipc.ForService(root, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(endpoint.Directory); !os.IsNotExist(err) {
		t.Fatal("TLS runtime fixture already exists")
	}
	t.Cleanup(func() { _ = os.RemoveAll(endpoint.Directory) })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, root, io.Discard) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			t.Error("TLS gateway failed to stop")
		}
	})
	for {
		status, err := GetStatus(ctx, root)
		if err == nil && status.Running {
			if status.URL != "https://"+u.Host {
				t.Fatalf("TLS service advertised %s", status.URL)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("TLS gateway did not start")
		case <-time.After(20 * time.Millisecond):
		}
	}
	response, err := client.Get("https://" + u.Host + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var metadata struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(response.Body).Decode(&metadata); err != nil || response.StatusCode != 200 || metadata.Issuer != "https://"+u.Host {
		t.Fatalf("TLS metadata=%+v status=%d err=%v", metadata, response.StatusCode, err)
	}
	if err := Stop(ctx, root); err != nil {
		t.Fatal(err)
	}
}
