package main

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestConnectBrowserOptionsRequireExplicitOptIn(t *testing.T) {
	var diagnostics bytes.Buffer
	called := 0
	opts := connectAuthorizationOptions(false, &diagnostics, func(string) error { called++; return nil })
	if opts.AuthorizationCodeFetcher != nil || called != 0 || diagnostics.Len() != 0 {
		t.Fatal("default stdio options enable browser authorization")
	}
	redirect, address := connectOAuthCallback(t)
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	opts = connectAuthorizationOptions(true, &diagnostics, func(string) error {
		called++
		connectOAuthRequest(t, client, http.MethodGet, redirect+"?state=opt-in&code=fixture-code", http.StatusOK)
		return nil
	})
	if opts.AuthorizationCodeFetcher == nil {
		t.Fatal("explicit opt-in did not enable the callback")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := opts.AuthorizationCodeFetcher(ctx, redirect, &sdkauth.AuthorizationArgs{URL: "https://issuer.invalid/authorize?state=opt-in"})
	if err != nil || result == nil || result.Code != "fixture-code" || called != 1 {
		t.Fatalf("opt-in callback failed: calls=%d err=%v", called, err)
	}
	assertConnectOAuthCallbackClosed(t, address)
}
