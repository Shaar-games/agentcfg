package upstream

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/wire"

	"golang.org/x/oauth2"
)

func TestHTTPFailedDiscoveryIgnoresLateCancellation(t *testing.T) {
	for _, failure := range []string{"oauth", "http"} {
		for _, next := range []string{"discover", "initialize"} {
			t.Run(failure+"/"+next, func(t *testing.T) {
				var discoveries, authorizations, cancellationPosts atomic.Int32
				transport := newHTTPFixture(t, normalHTTPHandler(t, func(w http.ResponseWriter, r *http.Request, message wire.Message) {
					switch message.Method() {
					case "server/discover":
						if discoveries.Add(1) == 1 {
							if failure == "oauth" {
								w.Header().Set("WWW-Authenticate", `Bearer realm="fixture"`)
								w.WriteHeader(http.StatusUnauthorized)
							} else {
								w.WriteHeader(http.StatusServiceUnavailable)
							}
							return
						}
						writeHTTPJSON(w, wire.Result(message.ID(), map[string]any{"supportedVersions": []string{wire.ModernProtocolVersion}, "capabilities": map[string]any{}}))
					case "notifications/cancelled":
						cancellationPosts.Add(1)
						w.WriteHeader(http.StatusAccepted)
					default:
						t.Errorf("unexpected fixture method: %s", message.Method())
						w.WriteHeader(http.StatusBadRequest)
					}
				}))
				if failure == "oauth" {
					transport.headers.Del("Authorization")
					transport.oauth = testOAuthHandler{
						tokenSource: func(context.Context) (oauth2.TokenSource, error) {
							return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fixture-token", TokenType: "Bearer"}), nil
						},
						authorize: func(context.Context, *http.Request, *http.Response) error {
							authorizations.Add(1)
							return auth.ErrLoginRequired
						},
					}
				}
				if err := transport.Write(modernHTTPMessage("probe", "server/discover", nil)); err != nil {
					t.Fatal(err)
				}
				response := readRoutedWithin(t, transport).message
				if string(response.ID()) != `"probe"` || response["error"] == nil {
					t.Fatalf("discovery failure was not correlated: %#v", response)
				}
				transport.mu.Lock()
				state := transport.state
				transport.mu.Unlock()
				if state != httpNew {
					t.Fatalf("failed discovery left transport in state %v", state)
				}

				// The broker can queue this advisory cancellation while the failed
				// HTTP exchange is unwinding. Once its result is available, the old
				// request no longer exists and cancellation must remain harmless.
				if err := transport.Write(wire.Notification("notifications/cancelled", map[string]any{"requestId": "probe"})); err != nil {
					t.Fatalf("late cancellation rejected after discovery reset: %v", err)
				}
				if next == "initialize" {
					initializeHTTP(t, transport)
				} else {
					if err := transport.Write(modernHTTPMessage("retry", "server/discover", nil)); err != nil {
						t.Fatalf("late cancellation prevented another discovery: %v", err)
					}
					if response := readRoutedWithin(t, transport).message; response["error"] != nil || response["result"] == nil {
						t.Fatalf("discovery retry failed: %#v", response)
					}
				}
				closeTransport(t, transport)
				if cancellationPosts.Load() != 0 {
					t.Fatal("late cancellation was forwarded after its request had completed")
				}
				if failure == "oauth" && authorizations.Load() != 1 {
					t.Fatalf("OAuth callbacks = %d, want one failed authorization", authorizations.Load())
				}
			})
		}
	}
}

func TestHTTPInitializeIgnoresCancellationWhilePending(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseInitialize := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseInitialize()
	var cancellationPosts atomic.Int32
	transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		message := decodeHTTP(t, r)
		switch message.Method() {
		case "initialize":
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			writeHTTPJSON(w, wire.Result(message.ID(), map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}}))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "notifications/cancelled":
			cancellationPosts.Add(1)
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected fixture method: %s", message.Method())
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	if err := transport.Write(rpcRequest("init", "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP initialize did not start")
	}
	if err := transport.Write(wire.Notification("notifications/cancelled", map[string]any{"requestId": "init"})); err != nil {
		t.Fatalf("invalid initialization cancellation was not ignored: %v", err)
	}
	releaseInitialize()
	if response := readRoutedWithin(t, transport).message; response["error"] != nil || response["result"] == nil {
		t.Fatalf("initialization was interrupted by its cancellation: %#v", response)
	}
	if err := transport.Write(wire.Notification("notifications/initialized", nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("initialization did not complete after the ignored cancellation")
	}
	closeTransport(t, transport)
	if cancellationPosts.Load() != 0 {
		t.Fatal("initialize cancellation reached the upstream MCP server")
	}
}
