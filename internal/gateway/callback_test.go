package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func proxyTestCallback(t *testing.T) (string, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return "http://" + address + "/callback", address
}

func TestProxyCallbackValidationAndCleanup(t *testing.T) {
	redirect, address := proxyTestCallback(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type outcome struct {
		result *sdkauth.AuthorizationResult
		err    error
	}
	done := make(chan outcome, 1)
	presented := make(chan struct{})
	go func() {
		result, err := proxyFetchAuthorization(ctx, redirect, &sdkauth.AuthorizationArgs{URL: "https://issuer.invalid/authorize?state=fixture-state"}, func(string) error { close(presented); return nil }, "http://127.0.0.1:39001/oauth/complete?ticket=opaque")
		done <- outcome{result, err}
	}()
	select {
	case <-presented:
	case <-ctx.Done():
		t.Fatal("authorization was not presented")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, tc := range []struct {
		method, query, host string
		status              int
	}{
		{http.MethodPost, "state=fixture-state&code=fixture-code", "", 405},
		{http.MethodGet, "state=wrong&code=fixture-code", "", 400},
		{http.MethodGet, "state=fixture-state&code=a&code=b", "", 400},
		{http.MethodGet, "state=fixture-state&code=fixture-code", "malicious.invalid", 404},
	} {
		request, _ := http.NewRequestWithContext(ctx, tc.method, redirect+"?"+tc.query, nil)
		if tc.host != "" {
			request.Host = tc.host
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != tc.status {
			t.Fatalf("invalid callback status=%d want=%d", response.StatusCode, tc.status)
		}
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, redirect+"?state=fixture-state&code=fixture-code&iss=https%3A%2F%2Fissuer.invalid", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 303 || response.Header.Get("Location") != "http://127.0.0.1:39001/oauth/complete?ticket=opaque" || strings.Contains(response.Header.Get("Location"), "fixture-code") {
		t.Fatal("callback leaked the upstream code or did not return to the local flow")
	}
	select {
	case got := <-done:
		if got.err != nil || got.result == nil || got.result.Code != "fixture-code" || got.result.State != "fixture-state" || got.result.Iss != "https://issuer.invalid" {
			t.Fatalf("callback result invalid: %v", got.err)
		}
	case <-ctx.Done():
		t.Fatal("callback did not complete")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal("callback listener was not released")
	}
	_ = listener.Close()
}

func TestProxyCallbackBindsBeforePresentationAndCancels(t *testing.T) {
	t.Run("occupied-port", func(t *testing.T) {
		occupied, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer occupied.Close()
		called := false
		_, err = proxyFetchAuthorization(t.Context(), "http://"+occupied.Addr().String()+"/callback", &sdkauth.AuthorizationArgs{URL: "https://issuer.invalid/authorize?state=fixture"}, func(string) error { called = true; return nil }, "http://127.0.0.1:39001/complete")
		if err == nil || called {
			t.Fatal("browser presentation happened before callback binding")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		redirect, address := proxyTestCallback(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		presented := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := proxyFetchAuthorization(ctx, redirect, &sdkauth.AuthorizationArgs{URL: "https://issuer.invalid/authorize?state=fixture"}, func(string) error { close(presented); return nil }, "http://127.0.0.1:39001/complete")
			done <- err
		}()
		select {
		case <-presented:
		case <-time.After(3 * time.Second):
			t.Fatal("presentation never started")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation classification lost: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("callback survived cancellation")
		}
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatal("cancelled callback listener was not released")
		}
		_ = listener.Close()
	})
	t.Run("presentation-error", func(t *testing.T) {
		redirect, _ := proxyTestCallback(t)
		_, err := proxyFetchAuthorization(t.Context(), redirect, &sdkauth.AuthorizationArgs{URL: "https://issuer.invalid/authorize?state=fixture"}, func(string) error { return errors.New("secret-code=private secret-token=private") }, "http://127.0.0.1:39001/complete")
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("presentation failure leaked its raw diagnostic")
		}
	})
}

func TestProxyCallbackRejectsUnsafeURLBeforePresentation(t *testing.T) {
	for _, raw := range []string{"https://issuer.invalid/authorize?state=fixture&access_token=secret", "https://user:secret@issuer.invalid/authorize?state=fixture", "http://public.invalid/authorize?state=fixture", "https://issuer.invalid/authorize?state=one&state=two"} {
		redirect, _ := proxyTestCallback(t)
		called := false
		_, err := proxyFetchAuthorization(t.Context(), redirect, &sdkauth.AuthorizationArgs{URL: raw}, func(string) error { called = true; return nil }, "http://127.0.0.1:39001/complete")
		if err == nil || called {
			t.Fatal("unsafe URL was presented")
		}
	}
	if proxyCompletionURL("http://127.0.0.1:39001/mcp/server", "http://127.0.0.1:39002/oauth/complete?ticket=valid") == nil {
		t.Fatal("completion crossed a local authority origin")
	}
	if err := proxyCompletionURL("http://127.0.0.1:39001/mcp/server", "http://127.0.0.1:39001/oauth/complete?ticket="+url.QueryEscape("opaque")); err != nil {
		t.Fatal(err)
	}
}
