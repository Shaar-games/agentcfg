package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/auth"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func connectOAuthCallback(t *testing.T) (string, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve callback listener: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return "http://" + address + "/oauth/callback", address
}

func assertConnectOAuthCallbackClosed(t *testing.T, address string) {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("OAuth callback listener was not released: %v", err)
	}
	_ = listener.Close()
}

func connectOAuthRequest(t *testing.T, client *http.Client, method, target string, wantStatus int) {
	t.Helper()
	request, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != wantStatus {
		t.Fatalf("callback %s returned HTTP %d, want %d", method, response.StatusCode, wantStatus)
	}
}

func TestConnectOAuthBrowserReceivesValidatedCallbackWithoutTerminalInput(t *testing.T) {
	redirect, address := connectOAuthCallback(t)
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	var diagnostics bytes.Buffer
	opens := 0
	authorize := "https://issuer.example/authorize?state=unique-state"
	fetcher := connectAuthorizationCodeFetcher(&diagnostics, func(target string) error {
		opens++
		if target != authorize {
			t.Fatalf("opened unexpected authorization URL: %s", target)
		}
		// All of these must leave the flow waiting for the legitimate callback.
		connectOAuthRequest(t, client, http.MethodPost, redirect+"?state=unique-state&code=fixture", http.StatusMethodNotAllowed)
		connectOAuthRequest(t, client, http.MethodGet, redirect+"?state=wrong&code=fixture", http.StatusBadRequest)
		connectOAuthRequest(t, client, http.MethodGet, redirect+"?state=unique-state&state=other&code=fixture", http.StatusBadRequest)
		connectOAuthRequest(t, client, http.MethodGet, "http://"+address+"/wrong?state=unique-state&code=fixture", http.StatusNotFound)
		connectOAuthRequest(t, client, http.MethodGet, redirect+"?state=unique-state&code=private-code&iss="+url.QueryEscape("https://issuer.example"), http.StatusOK)
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	answer, err := fetcher(ctx, redirect, &sdkauth.AuthorizationArgs{URL: authorize})
	if err != nil {
		t.Fatal(err)
	}
	if opens != 1 || answer.Code != "private-code" || answer.State != "unique-state" || answer.Iss != "https://issuer.example" {
		t.Fatalf("unexpected browser callback result: opens=%d, result=%+v", opens, answer)
	}
	if !strings.Contains(diagnostics.String(), "Opening your browser") || strings.Contains(diagnostics.String(), authorize) || strings.Contains(diagnostics.String(), "Paste") || strings.Contains(diagnostics.String(), "private-code") {
		t.Fatalf("unexpected automatic login diagnostics: %q", diagnostics.String())
	}
	assertConnectOAuthCallbackClosed(t, address)
}

func TestConnectOAuthRejectsUnusableCallbackBeforeOpeningBrowser(t *testing.T) {
	cases := []struct {
		name, redirect, authorize string
	}{
		{"remote-callback", "https://client.example/callback", "https://issuer.example/authorize?state=expected"},
		{"https-loopback", "https://127.0.0.1:48761/callback", "https://issuer.example/authorize?state=expected"},
		{"missing-port", "http://127.0.0.1/callback", "https://issuer.example/authorize?state=expected"},
		{"zero-port", "http://127.0.0.1:0/callback", "https://issuer.example/authorize?state=expected"},
		{"callback-query", "http://127.0.0.1:48761/callback?old=state", "https://issuer.example/authorize?state=expected"},
		{"callback-fragment", "http://127.0.0.1:48761/callback#fragment", "https://issuer.example/authorize?state=expected"},
		{"callback-credentials", "http://user:secret@127.0.0.1:48761/callback", "https://issuer.example/authorize?state=expected"},
		{"missing-host", "http://127.0.0.1:48761/callback", "https:/authorize?state=expected"},
		{"wrong-scheme", "http://127.0.0.1:48761/callback", "ftp://127.0.0.1/authorize?state=expected"},
		{"missing-state", "http://127.0.0.1:48761/callback", "https://issuer.example/authorize"},
		{"repeated-state", "http://127.0.0.1:48761/callback", "https://issuer.example/authorize?state=one&state=two"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opened := false
			var diagnostics bytes.Buffer
			fetcher := connectAuthorizationCodeFetcher(&diagnostics, func(string) error { opened = true; return nil })
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := fetcher(ctx, tc.redirect, &sdkauth.AuthorizationArgs{URL: tc.authorize}); err == nil {
				t.Fatal("accepted unusable automatic OAuth callback")
			}
			if opened || !strings.Contains(diagnostics.String(), "OAuth") || strings.Contains(diagnostics.String(), tc.redirect) || strings.Contains(diagnostics.String(), tc.authorize) {
				t.Fatal("automatic OAuth navigated before callback validation or omitted a safe local diagnostic")
			}
		})
	}
	t.Run("occupied-port", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		opened := false
		var diagnostics bytes.Buffer
		fetcher := connectAuthorizationCodeFetcher(&diagnostics, func(string) error { opened = true; return nil })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err = fetcher(ctx, "http://"+listener.Addr().String()+"/callback", &sdkauth.AuthorizationArgs{URL: "https://issuer.example/authorize?state=expected"})
		if err == nil || opened {
			t.Fatalf("occupied callback port did not fail before navigation: opened=%t err=%v", opened, err)
		}
		if !strings.Contains(diagnostics.String(), "callback port is unavailable") || strings.Contains(diagnostics.String(), listener.Addr().String()) {
			t.Fatalf("callback bind failure omitted or leaked its diagnostic: %q", diagnostics.String())
		}
	})
}

func TestConnectOAuthFailuresReleaseCallbackListener(t *testing.T) {
	for _, failure := range []string{"diagnostics", "browser", "cancel", "timeout", "decline", "cancel-after-callback"} {
		t.Run(failure, func(t *testing.T) {
			redirect, address := connectOAuthCallback(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if failure == "timeout" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 250*time.Millisecond)
				defer deadlineCancel()
			}
			var diagnosticBuffer bytes.Buffer
			var diagnostics io.Writer = &diagnosticBuffer
			if failure == "diagnostics" {
				diagnostics = failingOutput{}
			}
			opens := 0
			browserFailure := errors.New("fixture browser failed: private-code secret-token https://callback.invalid/?code=private-code")
			fetcher := connectAuthorizationCodeFetcher(diagnostics, func(string) error {
				opens++
				switch failure {
				case "browser":
					return browserFailure
				case "cancel":
					cancel()
				case "decline", "cancel-after-callback":
					client := &http.Client{Timeout: time.Second}
					defer client.CloseIdleConnections()
					query := "state=expected&error=access_denied&error_description=private-provider-reason"
					if failure == "cancel-after-callback" {
						query = "state=expected&code=fixture-code"
					}
					connectOAuthRequest(t, client, http.MethodGet, redirect+"?"+query, http.StatusOK)
					if failure == "cancel-after-callback" {
						cancel()
					}
				}
				return nil
			})
			_, err := fetcher(ctx, redirect, &sdkauth.AuthorizationArgs{URL: "https://issuer.example/authorize?state=expected"})
			if err == nil {
				t.Fatal("automatic OAuth swallowed failure")
			}
			if failure != "diagnostics" {
				if !strings.Contains(diagnosticBuffer.String(), "OAuth") {
					t.Fatal("automatic OAuth failure produced no local diagnostic")
				}
				for _, secret := range []string{"private-code", "secret-token", "private-provider-reason", "https://", "state="} {
					if strings.Contains(diagnosticBuffer.String(), secret) {
						t.Fatalf("automatic OAuth diagnostic leaked %q: %s", secret, diagnosticBuffer.String())
					}
				}
			}
			switch failure {
			case "diagnostics":
				if opens != 0 || !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("diagnostics failure: opens=%d err=%v", opens, err)
				}
			case "browser":
				if !errors.Is(err, browserFailure) {
					t.Fatal(err)
				}
				if !strings.Contains(diagnosticBuffer.String(), "browser could not be opened") || strings.Contains(err.Error(), "secret-token") {
					t.Fatal("browser failure did not preserve a useful, fixed diagnostic")
				}
			case "cancel", "cancel-after-callback":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "timeout":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			case "decline":
				if !strings.Contains(err.Error(), "declined authorization") || strings.Contains(err.Error(), "private-provider-reason") {
					t.Fatal(err)
				}
			}
			assertConnectOAuthCallbackClosed(t, address)
		})
	}
}

func TestConnectOAuthCancelledBeforeStartDoesNotNavigate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fetcher := connectAuthorizationCodeFetcher(io.Discard, func(string) error {
		return fmt.Errorf("browser must not be invoked")
	})
	if _, err := fetcher(ctx, "http://127.0.0.1:48761/callback", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled flow: %v", err)
	}
}

func TestAuthStatusImplicitOAuthDoesNotNeedConfigurationEdits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTCFG_OAUTH_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	dir := filepath.Join(home, ".agentcfg")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mcp.json")
	before := []byte(`{"mcpServers":{"fixture":{"url":"https://mcp.example.test/mcp"}}}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runAuth(context.Background(), home, []string{"status", "fixture", "--json"}, nil, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var state auth.Status
	if err := json.Unmarshal(output.Bytes(), &state); err != nil || !state.Enabled || state.Authenticated {
		t.Fatalf("implicit OAuth status: %+v, %v", state, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("auth status modified canonical configuration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "credentials")); !os.IsNotExist(err) {
		t.Fatalf("auth status created a credential store: %v", err)
	}
}
