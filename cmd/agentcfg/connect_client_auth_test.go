package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/auth"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestConnectClientOAuthBindsCallbackBeforePresentation(t *testing.T) {
	redirect, address := connectOAuthCallback(t)
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	var diagnostics bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	url := "https://issuer.example/authorize?state=expected"
	presentations := 0
	result, err := connectClientAuthorizationCodeFetcher(&diagnostics)(ctx, redirect, &sdkauth.AuthorizationArgs{URL: url}, func(presentation context.Context, target string) error {
		presentations++
		if presentation.Err() != nil || target != url {
			t.Fatalf("unexpected presentation: target=%s err=%v", target, presentation.Err())
		}
		// A client can finish the browser interaction before it acknowledges the
		// request. The callback must already be listening, and must validate state.
		connectOAuthRequest(t, client, http.MethodGet, redirect+"?state=wrong&code=private-code", http.StatusBadRequest)
		connectOAuthRequest(t, client, http.MethodGet, redirect+"?state=expected&code=private-code", http.StatusOK)
		return nil
	})
	if err != nil || result == nil || result.Code != "private-code" || result.State != "expected" || presentations != 1 {
		t.Fatalf("client authorization failed: presentations=%d result=%+v err=%v", presentations, result, err)
	}
	if !strings.Contains(diagnostics.String(), "MCP client") || strings.Contains(diagnostics.String(), "Opening your browser") {
		t.Fatalf("wrong presentation diagnostic: %q", diagnostics.String())
	}
	for _, secret := range []string{url, "private-code", "state=expected"} {
		if strings.Contains(diagnostics.String(), secret) {
			t.Fatalf("client diagnostic exposed %q", secret)
		}
	}
	assertConnectOAuthCallbackClosed(t, address)
}

func TestConnectClientOAuthPresentationFailuresReleaseCallback(t *testing.T) {
	privateFailure := errors.New("private-provider-body code=secret-token")
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"decline", context.Canceled},
		{"timeout", context.DeadlineExceeded},
		{"manual-login", auth.ErrLoginRequired},
		{"presentation-error", privateFailure},
	} {
		t.Run(failure.name, func(t *testing.T) {
			redirect, address := connectOAuthCallback(t)
			var diagnostics bytes.Buffer
			result, err := connectClientAuthorizationCodeFetcher(&diagnostics)(t.Context(), redirect,
				&sdkauth.AuthorizationArgs{URL: "https://issuer.example/authorize?state=expected"},
				func(context.Context, string) error { return failure.err })
			if result != nil || !errors.Is(err, failure.err) {
				t.Fatalf("presentation failure lost classification: result=%+v err=%v", result, err)
			}
			for _, secret := range []string{"secret-token", "private-provider-body", "state=expected"} {
				if strings.Contains(diagnostics.String(), secret) || strings.Contains(err.Error(), secret) {
					t.Fatalf("presentation failure leaked %q", secret)
				}
			}
			assertConnectOAuthCallbackClosed(t, address)
		})
	}
}

func TestConnectClientOAuthMissingPresentationDoesNotBindCallback(t *testing.T) {
	redirect, address := connectOAuthCallback(t)
	result, err := connectClientAuthorizationCodeFetcher(nil)(t.Context(), redirect,
		&sdkauth.AuthorizationArgs{URL: "https://issuer.example/authorize?state=expected"}, nil)
	if result != nil || !errors.Is(err, auth.ErrLoginRequired) {
		t.Fatalf("missing client presenter: result=%+v err=%v", result, err)
	}
	assertConnectOAuthCallbackClosed(t, address)
}

func TestConnectClientOAuthAcceptedWithoutCallbackHonorsCancellation(t *testing.T) {
	redirect, address := connectOAuthCallback(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var diagnostics bytes.Buffer
	result, err := connectClientAuthorizationCodeFetcher(&diagnostics)(ctx, redirect,
		&sdkauth.AuthorizationArgs{URL: "https://issuer.example/authorize?state=expected"},
		func(context.Context, string) error {
			cancel()
			return nil
		})
	if result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("dismissed client flow kept waiting: result=%+v err=%v", result, err)
	}
	assertConnectOAuthCallbackClosed(t, address)
}
