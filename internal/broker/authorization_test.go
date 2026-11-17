package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestOAuthIPCDoesNotTrustUpstreamControlFields(t *testing.T) {
	b, _ := setupBackend(t, true, time.Second)
	server, client := net.Pipe()
	p := newPeer(server)
	p.authorizes = true
	if err := b.attach(p); err != nil {
		t.Fatal(err)
	}
	go p.writeLoop()
	t.Cleanup(func() { p.close(); b.detach(p); _ = client.Close() })
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	// Even in transparent isolated mode, an untrusted upstream cannot forge
	// a control frame: its complete message stays inside the MCP envelope.
	untrusted := wire.Message{"authorize": wire.Raw(authorizationRequest{
		ID: "injected", URL: "https://attacker.invalid/authorize", RedirectURL: "http://127.0.0.1:9999/callback",
	})}
	b.routeFrom(untrusted, nil)
	envelope, err := wire.NewReaderLimit(client, maxIPCFrameSize).Read()
	if err != nil {
		t.Fatal(err)
	}
	if envelope["authorize"] != nil || envelope["mcp"] == nil {
		t.Fatal("upstream escaped its MCP envelope")
	}
	message, err := decodeMCPEnvelope(envelope)
	if err != nil || !bytes.Equal(message["authorize"], untrusted["authorize"]) {
		t.Fatal("transparent upstream message was not preserved")
	}
	// The reverse direction has the same boundary: an MCP client cannot
	// submit an authorization response just by using a reserved field name.
	clientMessage := wire.Message{"authorization": wire.Raw(authorizationResponse{ID: "injected", Error: "disconnected"})}
	wrapped, err := encodeMCPEnvelope(clientMessage)
	if err != nil || wrapped["authorization"] != nil || wrapped["mcp"] == nil {
		t.Fatal("client escaped its MCP envelope")
	}
}

func TestOAuthIPCEnvelopePreservesMaximumMCPFrame(t *testing.T) {
	message := wire.Result(wire.Raw(1), "")
	empty, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	message["result"] = wire.Raw(strings.Repeat("x", wire.MaxFrameSize-len(empty)))
	envelope, err := encodeMCPEnvelope(message)
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := wire.NewWriterLimit(&stream, maxIPCFrameSize).Write(envelope); err != nil {
		t.Fatalf("private envelope rejected a valid maximum-size MCP frame: %v", err)
	}
	received, err := wire.NewReaderLimit(&stream, maxIPCFrameSize).Read()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMCPEnvelope(received)
	if err != nil {
		t.Fatal(err)
	}
	stream.Reset()
	if err := wire.NewWriter(&stream).Write(decoded); err != nil || stream.Len() != wire.MaxFrameSize+1 {
		t.Fatalf("MCP frame budget changed: bytes=%d err=%v", stream.Len(), err)
	}
}

func TestOAuthIPCRejectsUnexpectedBrowserInstructions(t *testing.T) {
	valid := authorizationRequest{ID: "oauth-1", URL: "https://issuer.invalid/authorize", RedirectURL: "http://127.0.0.1:49152/oauth/callback"}
	for _, name := range []string{"extra field", "wrong redirect", "empty id", "missing request", "oversized url"} {
		t.Run(name, func(t *testing.T) {
			request := valid
			envelope := wire.Message{}
			switch name {
			case "extra field":
				envelope["mcp"] = wire.Raw(map[string]any{})
			case "wrong redirect":
				request.RedirectURL = "http://127.0.0.1:49153/oauth/callback"
			case "empty id":
				request.ID = ""
			case "oversized url":
				request.URL = strings.Repeat("x", 16385)
			}
			if name != "missing request" {
				envelope["authorize"] = wire.Raw(request)
			}
			control := &clientAuthorization{ctx: context.Background(), redirect: valid.RedirectURL,
				flows: make(map[string]context.CancelFunc), opts: ConnectOptions{
					AuthorizationCodeFetcher: func(context.Context, string, *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
						t.Error("invalid broker control opened a browser")
						return nil, errors.New("unexpected browser")
					},
				}}
			if err := control.handle(envelope); err == nil {
				t.Fatal("invalid control accepted")
			}
			control.close()
		})
	}
}

func TestOAuthIPCRedactsCallbackFailure(t *testing.T) {
	var output bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := &clientAuthorization{ctx: ctx, redirect: "http://127.0.0.1:49152/oauth/callback",
		writer: wire.NewWriterLimit(&output, maxIPCFrameSize), flows: make(map[string]context.CancelFunc),
		opts: ConnectOptions{AuthorizationCodeFetcher: func(context.Context, string, *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
			return nil, errors.New("private-code secret-token provider-body")
		}}}
	err := control.handle(wire.Message{"authorize": wire.Raw(authorizationRequest{
		ID: "oauth-1", URL: "https://issuer.invalid/authorize", RedirectURL: control.redirect,
	})})
	if err != nil {
		t.Fatal(err)
	}
	control.wg.Wait()
	control.close()
	if strings.Contains(output.String(), "private-code") || strings.Contains(output.String(), "secret-token") {
		t.Fatal("callback failure leaked its underlying error")
	}
	message, err := wire.NewReader(&output).Read()
	if err != nil {
		t.Fatal(err)
	}
	var response authorizationResponse
	if json.Unmarshal(message["authorization"], &response) != nil || response.Error != "failed" || response.Result != nil {
		t.Fatal("unexpected callback error classification")
	}
}

func TestOAuthIPCUserCancellationDoesNotMoveToAnotherPeer(t *testing.T) {
	server, client := net.Pipe()
	p := newPeer(server)
	p.authorizes = true
	go p.writeLoop()
	defer p.close()
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	relay := newAuthorizationRelay("http://127.0.0.1:49152/oauth/callback")
	relay.attach(p)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := relay.fetch(ctx, &sdkauth.AuthorizationArgs{URL: "https://issuer.invalid/authorize"})
		finished <- err
	}()
	message, err := wire.NewReader(client).Read()
	if err != nil {
		t.Fatal(err)
	}
	var request authorizationRequest
	if json.Unmarshal(message["authorize"], &request) != nil || request.ID == "" {
		t.Fatal("authorization request missing")
	}
	if err := p.authorizationResponse(wire.Raw(authorizationResponse{ID: request.ID, Error: "cancelled"})); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("user cancellation was retried: %v", err)
	}
}
