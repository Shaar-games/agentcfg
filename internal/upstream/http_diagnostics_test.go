package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/config"
	"agentcfg/internal/wire"
)

// Exercise the SDK boundary, not just an error formatter: the SDK wraps write
// failures and flattens some asynchronous response failures into strings.
func TestHTTPStatusDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{401, "authentication"},
		{403, "access denied"},
		{404, "endpoint not found"},
		{405, "method not allowed"},
		{407, "proxy authentication"},
		{429, "rate limit"},
		{503, "server error"},
		{307, "redirect rejected"},
		{599, "server error"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			var posts, redirects atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				redirects.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(target.Close)
			transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				w.Header().Set("Location", target.URL+"/location-secret?token=location-secret")
				w.Header().Set("WWW-Authenticate", `Bearer error_description="challenge-secret"`)
				w.Header().Set("Content-Type", "text/plain; private=content-type-secret")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "body-secret")
			}))
			if err := transport.Write(rpcRequest("init", "initialize", map[string]any{"protocolVersion": "2025-11-25"})); err != nil {
				t.Fatal(err)
			}
			message := mustRead(t, transport)
			assertHTTPDiagnostic(t, message, "init", tc.want, fmt.Sprintf("HTTP %d", tc.status))
			closeTransport(t, transport)
			if posts.Load() != 1 || redirects.Load() != 0 {
				t.Fatalf("unexpected requests: %d original, %d redirected", posts.Load(), redirects.Load())
			}
		})
	}
}

func TestHTTPNetworkDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  string
	}{
		{"dns", &net.DNSError{Err: "network-secret", Name: "host-secret.invalid", Server: "resolver-secret", IsNotFound: true}, "DNS lookup failed"},
		{"certificate", &tls.CertificateVerificationError{Err: x509.HostnameError{Certificate: &x509.Certificate{}, Host: "certificate-secret.invalid"}}, "TLS certificate verification failed"},
		{"connection", &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}, Err: errors.New("network-secret")}, "network connection failed"},
		{"network-timeout", &net.DNSError{Err: "network-secret", Name: "host-secret.invalid", IsTimeout: true}, "request timed out"},
		{"deadline", context.DeadlineExceeded, "request timed out"},
		{"cancelled", context.Canceled, "request cancelled"},
		{"untyped", errors.New("network-secret HTTP 401 authentication"), "request failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := startTestTransport(t, context.Background(), config.Server{Type: "http", URL: "https://user-secret:password-secret@host-secret.invalid/mcp?token=url-secret"}).(*httpTransport)
			var attempts atomic.Int32
			transport.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempts.Add(1)
				return nil, &url.Error{Op: "Post", URL: request.URL.String(), Err: tc.cause}
			})
			if err := transport.Write(rpcRequest("network", "initialize", map[string]any{"protocolVersion": "2025-11-25"})); err != nil {
				t.Fatal(err)
			}
			message := mustRead(t, transport)
			assertHTTPDiagnostic(t, message, "network", tc.want)
			closeTransport(t, transport)
			if attempts.Load() != 1 {
				t.Fatalf("request replayed %d times", attempts.Load())
			}
		})
	}
}

func TestHTTPResponseDiagnostic(t *testing.T) {
	transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"body-secret":`)
	}))
	if err := transport.Write(rpcRequest("response", "initialize", map[string]any{"protocolVersion": "2025-11-25"})); err != nil {
		t.Fatal(err)
	}
	assertHTTPDiagnostic(t, mustRead(t, transport), "response", "response was invalid or interrupted", "HTTP 200")
}

func TestHTTPConcurrentDiagnosticsStayWithTheirRequest(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var posts atomic.Int32
	transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		message := decodeHTTP(t, r)
		posts.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		if string(message.ID()) == `"auth"` {
			w.WriteHeader(http.StatusUnauthorized)
		} else {
			w.WriteHeader(http.StatusTooManyRequests)
		}
		_, _ = io.WriteString(w, "body-secret")
	}))
	t.Cleanup(unblock)
	for _, id := range []string{"auth", "limit"} {
		if err := transport.Write(modernHTTPMessage(id, "tools/list", nil)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for range 2 {
		select {
		case <-entered:
		case <-deadline.C:
			t.Fatal("HTTP requests did not overlap")
		}
	}
	unblock()
	seen := make(map[string]bool)
	for range 2 {
		message := mustRead(t, transport)
		var id string
		if err := json.Unmarshal(message.ID(), &id); err != nil || seen[id] {
			t.Fatalf("invalid or repeated response ID: %s", message.ID())
		}
		seen[id] = true
		switch id {
		case "auth":
			assertHTTPDiagnostic(t, message, id, "HTTP 401", "authentication")
		case "limit":
			assertHTTPDiagnostic(t, message, id, "HTTP 429", "rate limit")
		default:
			t.Fatalf("unexpected response ID: %q", id)
		}
	}
	closeTransport(t, transport)
	if posts.Load() != 2 {
		t.Fatalf("requests were replayed: %d POSTs", posts.Load())
	}
}

func assertHTTPDiagnostic(t *testing.T, message wire.Message, id string, want ...string) {
	t.Helper()
	var rpcError struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(message["error"], &rpcError); err != nil || rpcError.Code != -32000 || string(message.ID()) != string(wire.Raw(id)) {
		t.Fatalf("missing correlated transport error: %s", message)
	}
	for _, text := range want {
		if !strings.Contains(rpcError.Message, text) {
			t.Errorf("diagnostic %q does not contain %q", rpcError.Message, text)
		}
	}
	encoded, _ := json.Marshal(message)
	for _, private := range []string{"secret", "127.0.0.1", "http://", "https://"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("diagnostic exposed private data: %s", encoded)
		}
	}
}
