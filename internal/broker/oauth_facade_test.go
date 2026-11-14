package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestOAuthFacadeAuthorizationEligibility(t *testing.T) {
	for _, tc := range []struct {
		name      string
		active    bool
		url       bool
		tool      bool
		auto      bool
		decline   bool
		wantURL   int32
		wantAuto  int32
		wantError error
	}{
		{name: "idle-even-with-opt-in", auto: true, wantError: ErrAuthorizationUnavailable},
		{name: "unsupported-default", active: true, tool: true, wantError: ErrAuthorizationUnavailable},
		{name: "url-probe-does-not-open", active: true, url: true, auto: true, wantError: ErrAuthorizationUnavailable},
		{name: "unsupported-opt-in-probe", active: true, auto: true, wantAuto: 1},
		{name: "url-wins-over-opt-in", active: true, url: true, tool: true, auto: true, wantURL: 1},
		{name: "decline-never-falls-back", active: true, url: true, tool: true, auto: true, decline: true, wantURL: 1, wantError: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var urlCalls, browserCalls atomic.Int32
			opts := ConnectOptions{
				AuthorizationCodeFetcher: func(context.Context, string, *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
					browserCalls.Add(1)
					return &sdkauth.AuthorizationResult{Code: "fixture-code"}, nil
				},
				ClientAuthorizationCodeFetcher: func(context.Context, string, *sdkauth.AuthorizationArgs, func(context.Context, string) error) (*sdkauth.AuthorizationResult, error) {
					urlCalls.Add(1)
					if tc.decline {
						return nil, context.Canceled
					}
					return &sdkauth.AuthorizationResult{Code: "fixture-code"}, nil
				},
			}
			f := newOAuthFacade(context.Background(), "fixture", opts, tc.auto, time.Minute)
			t.Cleanup(f.cancel)
			if tc.active {
				f.active = &facadeOperation{ctx: f.ctx, urlSupported: tc.url, allowUI: tc.tool}
			}
			_, err := f.fetchAuthorization(f.ctx, "http://127.0.0.1:9876/callback", &sdkauth.AuthorizationArgs{URL: "https://auth.example.invalid/authorize?state=fixture"})
			if !errors.Is(err, tc.wantError) || urlCalls.Load() != tc.wantURL || browserCalls.Load() != tc.wantAuto {
				t.Fatalf("authorization selection: error=%v URL=%d browser=%d", err, urlCalls.Load(), browserCalls.Load())
			}
		})
	}
}

func TestOAuthFacadeURLCapabilitiesAreExplicit(t *testing.T) {
	for raw, expected := range map[string]bool{
		`{}`: false, `{"elicitation":{}}`: false, `{"elicitation":{"form":{}}}`: false,
		`{"elicitation":{"url":null}}`: false, `{"elicitation":{"url":true}}`: false,
		`{"elicitation":{"url":{}}}`: true, `{"elicitation":{"form":{},"url":{}}}`: true,
	} {
		if got := facadeURLCapability(json.RawMessage(raw)); got != expected {
			t.Errorf("URL capability %s = %t, want %t", raw, got, expected)
		}
	}
}

func TestOAuthFacadeAuthorizationURLsExcludeCredentials(t *testing.T) {
	for target, expected := range map[string]bool{
		"https://auth.example.invalid/authorize?state=x&code_challenge=public": true,
		"http://127.0.0.1:9876/authorize?state=x":                              true,
		"http://[::1]:9876/authorize?state=x":                                  true,
		"http://auth.example.invalid/authorize":                                false,
		"https://user:password@auth.example.invalid/authorize":                 false,
		"https://auth.example.invalid/authorize#token":                         false,
		"https://auth.example.invalid/authorize?code=private":                  false,
		"https://auth.example.invalid/authorize?access_token=private":          false,
		"https://auth.example.invalid/authorize?refresh_token=private":         false,
		"https://auth.example.invalid/authorize?client_secret=private":         false,
	} {
		if got := facadeSafeAuthorizationURL(target); got != expected {
			t.Errorf("authorization URL accepted=%t, want %t for fixture URL", got, expected)
		}
	}
}

func TestOAuthFacadeModernContinuationIsBoundToOriginalCall(t *testing.T) {
	f := newOAuthFacade(context.Background(), "fixture", ConnectOptions{}, false, time.Minute)
	t.Cleanup(f.cancel)
	f.localToolAdvertised = true
	request := modernMessage("first", "tools/call", map[string]any{"name": facadeAuthenticationTool, "arguments": map[string]any{}})
	op := &facadeOperation{ctx: f.ctx, request: request, modern: true, urlSupported: true, allowUI: true,
		responseID: request.ID(), originalID: request.ID(), resume: make(chan wire.Message, 1), signature: facadeCallSignature(request)}
	f.operations[string(request.ID())] = op
	done := make(chan error, 1)
	go func() {
		done <- f.presentAuthorization(f.ctx, op, "https://auth.example.invalid/authorize?state=fixture")
	}()
	var response wire.Message
	select {
	case response = <-f.outgoing:
	case <-time.After(time.Second):
		t.Fatal("modern OAuth did not produce an input_required result")
	}
	var result struct {
		ResultType   string                     `json:"resultType"`
		RequestState string                     `json:"requestState"`
		Inputs       map[string]json.RawMessage `json:"inputRequests"`
	}
	if json.Unmarshal(response["result"], &result) != nil || result.ResultType != "input_required" || result.RequestState == "" || len(result.Inputs) != 1 {
		t.Fatal("invalid modern OAuth input request")
	}
	for _, invalid := range []struct {
		name string
		id   string
		args map[string]any
	}{
		{"same-id", "first", map[string]any{}},
		{"changed-arguments", "retry", map[string]any{"different": true}},
	} {
		retry := modernMessage(invalid.id, "tools/call", map[string]any{"name": facadeAuthenticationTool,
			"arguments": invalid.args, "requestState": result.RequestState,
			"inputResponses": map[string]any{"oauth": map[string]string{"action": "accept"}}})
		if handled, err := f.resumeAuthorization(retry, true); !handled || err != nil {
			t.Fatalf("invalid %s continuation was not handled: %v", invalid.name, err)
		}
		if failure := <-f.outgoing; facadeRPCCode(failure) != -32602 {
			t.Fatalf("invalid %s continuation was accepted", invalid.name)
		}
	}
	retry := modernMessage("accepted", "tools/call", map[string]any{"name": facadeAuthenticationTool, "arguments": map[string]any{},
		"requestState": result.RequestState, "inputResponses": map[string]any{"oauth": map[string]string{"action": "accept"}}})
	if handled, err := f.resumeAuthorization(retry, true); !handled || err != nil {
		t.Fatalf("valid continuation was rejected: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("valid OAuth continuation did not resume its callback")
	}
	if handled, err := f.resumeAuthorization(retry, true); !handled || err != nil {
		t.Fatalf("replayed local continuation was not handled: %v", err)
	}
	if failure := <-f.outgoing; facadeRPCCode(failure) != -32602 {
		t.Fatal("completed OAuth continuation was accepted twice")
	}
}

func TestOAuthFacadeDoesNotCaptureUpstreamContinuations(t *testing.T) {
	f := newOAuthFacade(context.Background(), "fixture", ConnectOptions{}, false, time.Minute)
	t.Cleanup(f.cancel)
	request := modernMessage("remote", "tools/call", map[string]any{"name": facadeAuthenticationTool, "arguments": map[string]any{},
		"requestState": "upstream-opaque", "inputResponses": map[string]any{"remote": map[string]string{"action": "accept"}}})
	if handled, err := f.resumeAuthorization(request, true); handled || err != nil {
		t.Fatalf("unadvertised authentication name shadowed upstream continuation: handled=%t error=%v", handled, err)
	}
	f.localToolAdvertised, f.remoteCatalog = true, true
	if handled, err := f.resumeAuthorization(request, true); handled || err != nil {
		t.Fatalf("active upstream tool was shadowed by local authentication: handled=%t error=%v", handled, err)
	}
}

func TestOAuthFacadeContinuationSignaturePreservesLargeNumbers(t *testing.T) {
	a := wire.Message{"method": wire.Raw("tools/call"), "params": json.RawMessage(`{"name":"fixture","arguments":{"a":900719925474099312345,"b":2}}`)}
	b := wire.Message{"method": wire.Raw("tools/call"), "params": json.RawMessage(`{"arguments":{"b":2,"a":900719925474099312345},"name":"fixture","requestState":"opaque","inputResponses":{}}`)}
	c := wire.Message{"method": wire.Raw("tools/call"), "params": json.RawMessage(`{"name":"fixture","arguments":{"a":900719925474099312346,"b":2}}`)}
	if facadeCallSignature(a) != facadeCallSignature(b) {
		t.Fatal("equivalent tool arguments changed the continuation signature")
	}
	if facadeCallSignature(a) == facadeCallSignature(c) {
		t.Fatal("distinct large numeric arguments shared a continuation signature")
	}
}

func TestOAuthFacadeModernProgressPrecedesResult(t *testing.T) {
	f := newOAuthFacade(context.Background(), "fixture", ConnectOptions{}, false, time.Minute)
	t.Cleanup(f.cancel)
	f.era = 2
	input, output := io.Pipe()
	t.Cleanup(func() { _ = input.Close(); _ = output.Close() })
	reply := make(chan wire.Message, 1)
	f.rpcReplies[`"upstream"`] = reply
	go f.readUpstream(input)
	go func() {
		select {
		case response := <-reply:
			response["id"] = wire.Raw("tool")
			_ = f.send(response)
		case <-f.ctx.Done():
		}
	}()
	writer := wire.NewWriter(output)
	// A modern server request is not a permitted replacement for MRTR.
	if err := writer.Write(wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw("callback"), "method": wire.Raw("elicitation/create"), "params": wire.Raw(map[string]any{})}); err != nil {
		t.Fatal(err)
	}
	progress := wire.Notification("notifications/progress", map[string]any{"progressToken": "client-progress", "progress": 5, "total": 10})
	if err := writer.Write(progress); err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(wire.Result(wire.Raw("upstream"), map[string]any{"resultType": "complete", "content": []any{}})); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-f.outgoing:
		if message.Method() != "notifications/progress" || string(message["params"]) != string(progress["params"]) {
			t.Fatalf("modern progress was lost or changed: %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("modern progress was not forwarded")
	}
	select {
	case message := <-f.outgoing:
		if string(message.ID()) != `"tool"` || message["result"] == nil {
			t.Fatalf("modern tool result did not follow its progress: %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("modern tool result did not follow its progress")
	}
}

type facadeCaptureWriter struct{ messages chan wire.Message }

func (w facadeCaptureWriter) Write(data []byte) (int, error) {
	message, err := wire.Decode(data)
	if err != nil {
		return 0, err
	}
	w.messages <- message
	return len(data), nil
}

func TestOAuthFacadeModernSubscriptionDoesNotBlockOrdinaryRequests(t *testing.T) {
	f := newOAuthFacade(context.Background(), "fixture", ConnectOptions{}, false, time.Minute)
	t.Cleanup(f.cancel)
	f.era, f.upstreamReady, f.remoteCatalog = 2, true, true
	messages := make(chan wire.Message, 8)
	f.upstream = wire.NewWriter(facadeCaptureWriter{messages: messages})
	listen := modernMessage("listen", "subscriptions/listen", nil)
	if err := f.startOperation(listen, true, false); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-messages:
		if message.Method() != "subscriptions/listen" {
			t.Fatal("subscription was not forwarded")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription did not start")
	}
	f.mu.Lock()
	operation := f.operations[string(listen.ID())]
	f.mu.Unlock()
	if operation == nil {
		t.Fatal("subscription disappeared before its stream completed")
	}
	if _, limited := operation.ctx.Deadline(); limited {
		t.Fatal("subscription inherited the ordinary request timeout")
	}
	for _, method := range []string{"subscriptions/acknowledge", "tools/list"} {
		if err := f.startOperation(modernMessage(method, method, nil), true, false); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]bool)
	for range 2 {
		select {
		case message := <-messages:
			seen[message.Method()] = true
			f.mu.Lock()
			reply := f.rpcReplies[string(message.ID())]
			f.mu.Unlock()
			if reply == nil {
				t.Fatal("ordinary request had no correlated reply")
			}
			reply <- wire.Result(message.ID(), map[string]any{"resultType": "complete", "tools": []any{}})
		case <-time.After(time.Second):
			t.Fatal("live subscription blocked acknowledge or tools/list")
		}
	}
	if !seen["subscriptions/acknowledge"] || !seen["tools/list"] {
		t.Fatalf("ordinary requests changed while subscription was active: %v", seen)
	}
	operation.cancel()
}

func TestOAuthFacadeToolTimeoutIsNotAnAuthenticationFailure(t *testing.T) {
	f := newOAuthFacade(context.Background(), "fixture", ConnectOptions{}, false, time.Minute)
	t.Cleanup(f.cancel)
	op := &facadeOperation{request: modernMessage("call", "tools/call", map[string]any{"name": "remote", "arguments": map[string]any{}}), modern: true}
	response := f.operationError(op, context.DeadlineExceeded)
	text := string(response["result"])
	if !strings.Contains(text, "timed out") || !strings.Contains(text, "not retried") || strings.Contains(text, "Authentication") || strings.Contains(text, "auth login") {
		t.Fatalf("ordinary tool timeout was presented as OAuth failure: %s", text)
	}
	op.authInProgress = true
	if response := f.operationError(op, context.DeadlineExceeded); !strings.Contains(string(response["result"]), "Authentication timed out") {
		t.Fatal("active OAuth timeout lost its authentication diagnosis")
	}
}

func TestOAuthFacadeInitializeWaitsForClientReadyBeforeUpstreamNotification(t *testing.T) {
	f := newOAuthFacade(context.Background(), "fixture", ConnectOptions{}, false, time.Minute)
	t.Cleanup(f.cancel)
	messages := make(chan wire.Message, 8)
	f.upstream = wire.NewWriter(facadeCaptureWriter{messages: messages})
	input, output := io.Pipe()
	t.Cleanup(func() { _ = input.Close(); _ = output.Close() })
	go f.readUpstream(input)
	writer := wire.NewWriter(output)
	initialize := wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw("client-init"), "method": wire.Raw("initialize"),
		"params": wire.Raw(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"roots": map[string]any{}}})}
	if err := f.handleClient(initialize); err != nil {
		t.Fatal(err)
	}
	var request wire.Message
	select {
	case request = <-messages:
		if request.Method() != "initialize" {
			t.Fatal("upstream did not receive initialization")
		}
	case <-time.After(time.Second):
		t.Fatal("initialization was not forwarded")
	}
	if err := writer.Write(wire.Result(request.ID(), map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "serverInfo": facadeServerInfo()})); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-f.outgoing:
		if string(response.ID()) != `"client-init"` || response["result"] == nil {
			t.Fatal("client did not receive the initialize result first")
		}
	case <-time.After(time.Second):
		t.Fatal("initialize result was not delivered")
	}
	// Writes to the private proxy are observed synchronously. This assertion is
	// independent of HTTP scheduling: initialized must not have been submitted.
	select {
	case message := <-messages:
		t.Fatalf("upstream was notified before the client was ready: %s", message.Method())
	default:
	}
	if err := f.handleClient(wire.Notification("notifications/initialized", nil)); err != nil {
		t.Fatal(err)
	}
	if message := <-messages; message.Method() != "notifications/initialized" {
		t.Fatalf("client readiness was not relayed: %s", message.Method())
	}
	// An isolated upstream can immediately ask its owning client for roots once
	// initialized. Preserve that callback and its response without a local ID mixup.
	callback := wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw("upstream-roots"), "method": wire.Raw("roots/list"), "params": wire.Raw(map[string]any{})}
	if err := writer.Write(callback); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-f.outgoing:
		if message.Method() != "roots/list" || string(message.ID()) != string(callback.ID()) {
			t.Fatalf("isolated callback was changed: %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("isolated callback was lost")
	}
	if err := f.handleClient(wire.Result(callback.ID(), map[string]any{"roots": []any{}})); err != nil {
		t.Fatal(err)
	}
	if response := <-messages; string(response.ID()) != string(callback.ID()) || response["result"] == nil {
		t.Fatal("isolated callback response was not relayed")
	}
}

func TestOAuthFacadeEveryExplicitDiscoveryReachesUpstream(t *testing.T) {
	f := newOAuthFacade(context.Background(), "fixture", ConnectOptions{}, false, time.Minute)
	t.Cleanup(f.cancel)
	messages := make(chan wire.Message, 8)
	f.upstream = wire.NewWriter(facadeCaptureWriter{messages: messages})
	for _, id := range []string{"first", "second"} {
		original := modernMessage(id, "server/discover", map[string]any{"extension": id})
		if err := f.handleClient(original); err != nil {
			t.Fatal(err)
		}
		var request wire.Message
		select {
		case request = <-messages:
			if request.Method() != "server/discover" || string(request["params"]) != string(original["params"]) {
				t.Fatalf("explicit discovery metadata changed: %#v", request)
			}
		case response := <-f.outgoing:
			t.Fatalf("cached discovery was returned without consulting the upstream: %#v", response)
		case <-time.After(time.Second):
			t.Fatal("explicit discovery was not forwarded")
		}
		f.mu.Lock()
		reply := f.rpcReplies[string(request.ID())]
		f.mu.Unlock()
		result := map[string]any{"resultType": "complete", "supportedVersions": []string{wire.ModernProtocolVersion},
			"capabilities": map[string]any{}, "instructions": id}
		reply <- wire.Result(request.ID(), result)
		select {
		case response := <-f.outgoing:
			if string(response.ID()) != string(original.ID()) || string(response["result"]) != string(wire.Raw(result)) {
				t.Fatalf("fresh discovery result was replaced: %#v", response)
			}
		case <-time.After(time.Second):
			t.Fatal("fresh discovery result was not forwarded")
		}
	}
}

func TestOAuthFacadeJSONRPCVersionUsesDecodedValue(t *testing.T) {
	for _, version := range []json.RawMessage{json.RawMessage(`"\u0032.0"`), json.RawMessage(`"2\u002e0"`)} {
		f := newOAuthFacade(context.Background(), "fixture", ConnectOptions{}, false, time.Minute)
		t.Cleanup(f.cancel)
		if err := f.handleClient(wire.Message{"jsonrpc": version, "id": wire.Raw("ping"), "method": wire.Raw("ping")}); err != nil {
			t.Fatal(err)
		}
		response := <-f.outgoing
		if response["error"] != nil || response["result"] == nil || string(response.ID()) != `"ping"` {
			t.Fatalf("valid escaped JSON-RPC version was rejected: %#v", response)
		}
	}
}
