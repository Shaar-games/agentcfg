package broker

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestOAuthIPCCancelWaitsForCallbackCleanupBeforeNextFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server, client := net.Pipe()
	p := newPeer(server)
	p.authorizes = true
	go p.writeLoop()
	started, cleanupStarted, cleanupRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(cleanupRelease) }) }
	var calls atomic.Int32
	control := &clientAuthorization{
		ctx: ctx, redirect: "http://127.0.0.1:49152/oauth/callback",
		writer: wire.NewWriterLimit(client, maxIPCFrameSize), flows: make(map[string]context.CancelFunc),
		opts: ConnectOptions{AuthorizationCodeFetcher: func(flow context.Context, _ string, _ *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-flow.Done()
				close(cleanupStarted)
				// Cancellation is observed before the browser callback releases its
				// listener. A new flow must not race this legitimate cleanup work.
				<-cleanupRelease
				return nil, flow.Err()
			}
			return &sdkauth.AuthorizationResult{Code: "next-flow-code", State: "next-flow-state"}, nil
		}},
	}
	t.Cleanup(func() {
		cancel()
		release()
		p.close()
		_ = client.Close()
		control.close()
	})
	// Run the actual private-envelope handlers in both directions. As with
	// ConnectWithOptions, a protocol error on either side closes the connection.
	go func() {
		defer client.Close()
		reader := wire.NewReaderLimit(client, maxIPCFrameSize)
		for {
			message, err := reader.Read()
			if err != nil || control.handle(message) != nil {
				return
			}
		}
	}()
	go func() {
		defer p.close()
		reader := wire.NewReaderLimit(server, maxIPCFrameSize)
		for {
			message, err := reader.Read()
			if err != nil || len(message) != 1 || message["authorization"] == nil || p.authorizationResponse(message["authorization"]) != nil {
				return
			}
		}
	}()
	first, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	firstReturned := make(chan error, 1)
	type outcome struct {
		result *sdkauth.AuthorizationResult
		err    error
	}
	nextReturned := make(chan outcome, 1)
	go func() {
		_, err := p.authorize(first, control.redirect, "https://issuer.example/authorize?state=first")
		firstReturned <- err
		// The credential-flight coordinator can start the replacement as soon
		// as authorize returns; the same IPC connection must remain usable.
		result, nextErr := p.authorize(ctx, control.redirect, "https://issuer.example/authorize?state=next")
		nextReturned <- outcome{result: result, err: nextErr}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first browser flow did not start")
	}
	cancelFirst()
	select {
	case <-cleanupStarted:
	case <-ctx.Done():
		t.Fatal("browser callback did not observe cancellation")
	}
	returnedEarly := false
	select {
	case err := <-firstReturned:
		returnedEarly = true
		t.Errorf("authorization returned before callback cleanup acknowledgement: %v", err)
	case <-time.After(150 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal("test context ended before cleanup could be checked")
	}
	release()
	if !returnedEarly {
		select {
		case err := <-firstReturned:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled authorization returned %v", err)
			}
		case <-ctx.Done():
			t.Fatal("authorization did not finish after callback cleanup")
		}
	}
	select {
	case got := <-nextReturned:
		if got.err != nil || got.result == nil || got.result.Code != "next-flow-code" || calls.Load() != 2 {
			t.Fatalf("next flow lost the reusable IPC connection: calls=%d result=%+v err=%v", calls.Load(), got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal("next authorization flow did not finish")
	}
}
