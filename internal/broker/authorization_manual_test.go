package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestOAuthIPCManualLoginRequiredKeepsItsClassification(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	server, client := net.Pipe()
	defer client.Close()
	p := newPeer(server)
	p.authorizes = true
	defer p.close()
	var output bytes.Buffer
	control := &clientAuthorization{ctx: ctx, redirect: "http://127.0.0.1:49152/oauth/callback",
		writer: wire.NewWriterLimit(&output, maxIPCFrameSize), flows: make(map[string]context.CancelFunc),
		opts: ConnectOptions{AuthorizationCodeFetcher: func(context.Context, string, *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
			return nil, auth.ErrLoginRequired
		}}}
	defer control.close()
	finished := make(chan error, 1)
	go func() {
		_, err := p.authorize(ctx, control.redirect, "https://issuer.example/authorize?state=expected")
		finished <- err
	}()
	select {
	case request := <-p.out:
		if err := control.handle(request); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("authorization request did not reach the local client")
	}
	control.wg.Wait()
	response, err := wire.NewReaderLimit(&output, maxIPCFrameSize).Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.authorizationResponse(response["authorization"]); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, auth.ErrLoginRequired) {
			t.Fatalf("manual login was misreported as an OAuth provider failure: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("manual login response was lost")
	}
}

func TestOAuthIPCUnavailableClientYieldsToReadyPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	firstServer, firstClient := net.Pipe()
	defer firstClient.Close()
	secondServer, secondClient := net.Pipe()
	defer secondClient.Close()
	first, second := newPeer(firstServer), newPeer(secondServer)
	first.authorizes, second.authorizes = true, true
	defer first.close()
	defer second.close()
	relay := newAuthorizationRelay("http://127.0.0.1:49152/oauth/callback")
	relay.attach(first)
	relay.attach(second)
	type outcome struct {
		result *sdkauth.AuthorizationResult
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := relay.fetch(ctx, &sdkauth.AuthorizationArgs{URL: "https://issuer.example/authorize?state=expected"})
		finished <- outcome{result, err}
	}()
	for index, p := range []*peer{first, second} {
		select {
		case envelope := <-p.out:
			var request authorizationRequest
			if err := json.Unmarshal(envelope["authorize"], &request); err != nil || request.ID == "" {
				t.Fatalf("peer %d did not receive an authorization request: %v", index, err)
			}
			response := authorizationResponse{ID: request.ID}
			if index == 0 {
				response.Error = "unavailable"
			} else {
				response.Result = &sdkauth.AuthorizationResult{Code: "ready-client-code", State: "expected"}
			}
			if err := p.authorizationResponse(wire.Raw(response)); err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatalf("authorization did not reach peer %d", index)
		}
	}
	select {
	case got := <-finished:
		if got.err != nil || got.result == nil || got.result.Code != "ready-client-code" {
			t.Fatalf("eligible peer could not finish shared authorization: %+v %v", got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal("shared authorization did not finish")
	}
}
