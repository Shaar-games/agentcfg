package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestOAuthManualRedirectURLInput(t *testing.T) {
	callback := "https://example.org/oauth/callback?state=my-state&code=abc&iss=https%3A%2F%2Fissuer.example"
	var output bytes.Buffer
	fetcher := authorizationCodeFetcher(
		"https://example.org/oauth/callback",
		strings.NewReader(callback+"\n"), &output, true,
	)
	answer, err := fetcher(context.Background(), &sdkauth.AuthorizationArgs{
		URL: "https://issuer.example/authorize?state=my-state",
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Code != "abc" || answer.State != "my-state" || answer.Iss != "https://issuer.example" {
		t.Fatalf("incorrect parsed OAuth redirect: %#v", answer)
	}
	if !strings.Contains(output.String(), "Paste the complete final redirected URL") {
		t.Fatal("manual input instructions missing")
	}
}

func TestOAuthTemporaryLoopbackCallback(t *testing.T) {
	availability, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listener unavailable: %v", err)
	}
	address := availability.Addr().String()
	_ = availability.Close()
	callback := "http://" + address + "/oauth/callback"
	var output bytes.Buffer
	fetcher := authorizationCodeFetcher(callback, nil, &output, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type outcome struct {
		result *sdkauth.AuthorizationResult
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := fetcher(ctx, &sdkauth.AuthorizationArgs{
			URL: "https://issuer.example/authorize?state=unique-state",
		})
		finished <- outcome{result: result, err: err}
	}()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	send := func(state string) (*http.Response, error) {
		return client.Get(callback + "?state=" + url.QueryEscape(state) + "&code=valid-code")
	}
	var bad *http.Response
	for attempt := 0; attempt < 40; attempt++ {
		bad, err = send("wrong-state")
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("callback listener unavailable: %v", err)
		case <-time.After(15 * time.Millisecond):
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, bad.Body)
	_ = bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("incorrect state was accepted (HTTP %d)", bad.StatusCode)
	}
	invalidPath, err := client.Get("http://" + address + "/wrong?state=unique-state&code=valid-code")
	if err != nil {
		t.Fatal(err)
	}
	_ = invalidPath.Body.Close()
	if invalidPath.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong callback path accepted: HTTP %d", invalidPath.StatusCode)
	}
	valid, err := send("unique-state")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, valid.Body)
	_ = valid.Body.Close()
	if valid.StatusCode != http.StatusOK {
		t.Fatalf("valid OAuth callback failed: HTTP %d", valid.StatusCode)
	}
	select {
	case got := <-finished:
		if got.err != nil || got.result.Code != "valid-code" || got.result.State != "unique-state" {
			t.Fatalf("loopback callback failed: %#v (%v)", got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal("loopback callback did not complete")
	}
	// The listener must be closed when the callback is consumed.
	if response, err := client.Get(callback + "?state=unique-state&code=replayed"); err == nil {
		_ = response.Body.Close()
		t.Log("listener may still be closing asynchronously")
	}
}

func TestOAuthAuthorizationURLValidation(t *testing.T) {
	for _, invalidURL := range []string{
		"http://example.com/authorize?state=test",
		"https://user:secret@example.com/authorize",
		"file:///etc/passwd",
	} {
		t.Run(fmt.Sprint(len(invalidURL)), func(t *testing.T) {
			fetcher := authorizationCodeFetcher(
				"https://example.com/callback", strings.NewReader(""), io.Discard, true,
			)
			_, err := fetcher(context.Background(), &sdkauth.AuthorizationArgs{URL: invalidURL})
			if err == nil {
				t.Fatalf("accepted unsafe OAuth authorization URL %q", invalidURL)
			}
		})
	}
}
