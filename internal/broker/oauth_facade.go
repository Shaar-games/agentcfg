package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/wire"
)

const facadeAuthenticationTool = "agentcfg_authenticate"
const maxFacadeOperations = 32
const maxFacadeOperationBytes = 2 * wire.MaxFrameSize

// oauthFacade owns the client-facing MCP session. Its upstream is the existing
// private broker proxy; OAuth grants, token exchange and storage stay there.
// Serializing upstream operations gives each private authorization request one
// unambiguous client request, including modern multi-round-trip continuations.
type oauthFacade struct {
	ctx         context.Context
	cancel      context.CancelFunc
	name        string
	opts        ConnectOptions
	autoBrowser bool
	timeout     time.Duration
	upstream    *wire.Writer
	outgoing    chan wire.Message
	finished    chan error
	gate        chan struct{}
	prefix      string
	sequence    atomic.Uint64

	mu                       sync.Mutex
	era                      int
	legacyReady              bool
	initializeComplete       bool
	initializeRequest        wire.Message
	legacyCapabilities       json.RawMessage
	announcedProtocol        string
	bootstrapHandshake       bool
	protocolMismatch         bool
	upstreamHandshake        wire.Message
	upstreamReady            bool
	upstreamNeedsInitialized bool
	remoteCatalog            bool
	localToolAdvertised      bool
	active                   *facadeOperation
	operations               map[string]*facadeOperation
	allOperations            map[*facadeOperation]bool
	operationBytes           int
	rpcReplies               map[string]chan wire.Message
	elicitationReplies       map[string]chan wire.Message
	continuations            map[string]*facadeOperation
}

type facadeOperation struct {
	ctx            context.Context
	cancel         context.CancelFunc
	request        wire.Message
	modern         bool
	urlSupported   bool
	allowUI        bool
	bytes          int
	responseID     json.RawMessage
	originalID     json.RawMessage
	responseSent   bool
	continuation   string
	resume         chan wire.Message
	signature      string
	loginNeeded    bool
	authError      error
	authInProgress bool
	authentication bool
	elicitation    string
}

type facadeRPCError struct {
	response wire.Message
	protocol bool
}

func (e *facadeRPCError) Error() string { return "upstream MCP request failed" }

func facadeOpaqueID(prefix string) string {
	var random [24]byte
	_, _ = rand.Read(random[:])
	return prefix + hex.EncodeToString(random[:])
}

func newOAuthFacade(ctx context.Context, name string, opts ConnectOptions, autoBrowser bool, timeout time.Duration) *oauthFacade {
	ctx, cancel := context.WithCancel(ctx)
	return &oauthFacade{ctx: ctx, cancel: cancel, name: name, opts: opts, autoBrowser: autoBrowser, timeout: timeout,
		outgoing: make(chan wire.Message, 16), finished: make(chan error, 4), gate: make(chan struct{}, 1),
		prefix: facadeOpaqueID("agentcfg-rpc-"), operations: make(map[string]*facadeOperation),
		allOperations: make(map[*facadeOperation]bool), rpcReplies: make(map[string]chan wire.Message),
		elicitationReplies: make(map[string]chan wire.Message), continuations: make(map[string]*facadeOperation)}
}

func connectOAuthFacade(ctx context.Context, root, name, harness string, input io.Reader, output io.Writer, opts ConnectOptions, autoBrowser bool, timeout time.Duration) error {
	f := newOAuthFacade(ctx, name, opts, autoBrowser, timeout)
	defer f.cancel()
	proxyInput, upstreamInput := io.Pipe()
	upstreamOutput, proxyOutput := io.Pipe()
	f.upstream = wire.NewWriter(upstreamInput)
	defer func() {
		f.cancel()
		_ = proxyInput.Close()
		_ = upstreamInput.Close()
		_ = upstreamOutput.Close()
		_ = proxyOutput.Close()
	}()
	go func() {
		err := connectStdioProxy(f.ctx, root, name, harness, proxyInput, proxyOutput,
			ConnectOptions{AuthorizationCodeFetcher: f.fetchAuthorization})
		_ = proxyOutput.CloseWithError(err)
		_ = proxyInput.CloseWithError(err)
		f.stop(err)
	}()
	go f.readUpstream(upstreamOutput)
	go func() {
		writer := wire.NewWriter(output)
		for {
			select {
			case message := <-f.outgoing:
				if err := writer.Write(message); err != nil {
					f.stop(err)
					return
				}
			case <-f.ctx.Done():
				return
			}
		}
	}()
	go func() {
		reader := wire.NewReader(input)
		for {
			message, err := reader.Read()
			if err != nil {
				f.stop(err)
				return
			}
			if err := f.handleClient(message); err != nil {
				f.stop(err)
				return
			}
		}
	}()
	select {
	case err := <-f.finished:
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *oauthFacade) stop(err error) {
	select {
	case f.finished <- err:
	default:
	}
	f.cancel()
}

func (f *oauthFacade) send(message wire.Message) error {
	select {
	case f.outgoing <- message:
		return nil
	case <-f.ctx.Done():
		return f.ctx.Err()
	}
}

func (f *oauthFacade) readUpstream(input io.Reader) {
	reader := wire.NewReader(input)
	for {
		message, err := reader.Read()
		if err != nil {
			f.stop(err)
			return
		}
		if message.Method() == "" && message.HasID() {
			f.mu.Lock()
			reply := f.rpcReplies[string(message.ID())]
			f.mu.Unlock()
			if reply != nil {
				select {
				case reply <- message:
				default:
				}
			}
			continue
		}
		f.mu.Lock()
		legacy := f.era == 1
		legacyReady := f.legacyReady
		modern := f.era == 2 || f.active != nil && f.active.modern
		f.mu.Unlock()
		if legacy && !legacyReady && message.Method() != "ping" && message.HasID() {
			// Initialization cannot start a client interaction. Pings remain
			// available before the client completes its own handshake.
			if err := f.upstream.Write(wire.Error(message.ID(), -32002, "MCP client initialization must complete before server callbacks")); err != nil {
				f.stop(err)
				return
			}
			continue
		}
		// The broker has already routed request-scoped notifications to their
		// owning peer. Modern MCP still carries those notifications, while its
		// server-to-client requests must use input_required results instead.
		if legacy || modern && !message.HasID() {
			if err := f.send(message); err != nil {
				return
			}
		}
	}
}

func (f *oauthFacade) call(op *facadeOperation, request wire.Message) (wire.Message, error) {
	if err := op.ctx.Err(); err != nil {
		return nil, err
	}
	request = request.Clone()
	id := wire.Raw(fmt.Sprintf("%s-%d", f.prefix, f.sequence.Add(1)))
	request["id"] = id
	reply := make(chan wire.Message, 1)
	f.mu.Lock()
	f.rpcReplies[string(id)] = reply
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.rpcReplies, string(id))
		f.mu.Unlock()
	}()
	if err := f.upstream.Write(request); err != nil {
		return nil, err
	}
	select {
	case result := <-reply:
		return result, nil
	case <-op.ctx.Done():
		if request.Method() != "initialize" {
			_ = f.upstream.Write(wire.Notification("notifications/cancelled", map[string]any{"requestId": id}))
		}
		return nil, op.ctx.Err()
	}
}

func (f *oauthFacade) ensureUpstream(op *facadeOperation) error {
	f.mu.Lock()
	ready, initialize := f.upstreamReady, f.initializeRequest.Clone()
	bootstrap, announced, mismatch := f.bootstrapHandshake, f.announcedProtocol, f.protocolMismatch
	f.mu.Unlock()
	if mismatch {
		return facadeProtocolMismatchError()
	}
	if ready {
		return nil
	}
	request := initialize
	if op.modern {
		if op.request.Method() == "server/discover" {
			request = op.request
		} else {
			request = facadeDiscoveryRequest(op.request)
		}
	}
	if !op.modern && request.Method() != "initialize" {
		return errors.New("MCP initialization is required")
	}
	result, err := f.call(op, request)
	if err != nil {
		return err
	}
	if result["error"] != nil {
		if bootstrap && op.modern && facadeRPCCode(result) == -32601 {
			f.mu.Lock()
			f.protocolMismatch = true
			f.mu.Unlock()
			return facadeProtocolMismatchError()
		}
		return &facadeRPCError{response: result}
	}
	if op.modern {
		var discovery struct {
			SupportedVersions []string `json:"supportedVersions"`
		}
		if json.Unmarshal(result["result"], &discovery) != nil || !facadeContains(discovery.SupportedVersions, wire.ModernProtocolVersion) {
			if bootstrap {
				f.mu.Lock()
				f.protocolMismatch = true
				f.mu.Unlock()
				return facadeProtocolMismatchError()
			}
			return &facadeRPCError{response: result, protocol: true}
		}
	} else {
		f.mu.Lock()
		clientReady := f.legacyReady
		if !clientReady {
			f.upstreamNeedsInitialized = true
		}
		f.mu.Unlock()
		if clientReady {
			if err := f.upstream.Write(wire.Notification("notifications/initialized", nil)); err != nil {
				return err
			}
		}
	}
	if !op.modern {
		var negotiated struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(result["result"], &negotiated) != nil || negotiated.ProtocolVersion == "" {
			return errors.New("upstream MCP initialization returned no protocol version")
		}
		if bootstrap && negotiated.ProtocolVersion != announced {
			f.mu.Lock()
			f.protocolMismatch = true
			f.mu.Unlock()
			return facadeProtocolMismatchError()
		}
	}
	f.mu.Lock()
	f.upstreamReady = true
	f.upstreamHandshake = result.Clone()
	op.loginNeeded = false
	op.authError = nil
	if op.modern && f.era == 0 {
		f.era = 2
	}
	f.mu.Unlock()
	return nil
}

func (f *oauthFacade) startOperation(message wire.Message, modern, urlSupported bool) error {
	size := 0
	for key, value := range message {
		size += len(key) + len(value)
	}
	f.mu.Lock()
	key := string(message.ID())
	if f.operations[key] != nil || len(f.allOperations) >= maxFacadeOperations || f.operationBytes+size > maxFacadeOperationBytes {
		f.mu.Unlock()
		return f.send(wire.Error(message.ID(), -32000, "MCP request is already pending or the local proxy is busy"))
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if modern && message.Method() == "subscriptions/listen" {
		ctx, cancel = context.WithCancel(f.ctx)
	} else {
		ctx, cancel = context.WithTimeout(f.ctx, f.timeout)
	}
	op := &facadeOperation{ctx: ctx, cancel: cancel, request: message.Clone(), modern: modern, urlSupported: urlSupported,
		allowUI: message.Method() == "tools/call" && facadeValidToolCall(message), bytes: size,
		responseID: append(json.RawMessage(nil), message.ID()...), originalID: append(json.RawMessage(nil), message.ID()...),
		resume: make(chan wire.Message, 1), signature: facadeCallSignature(message)}
	f.operations[key], f.allOperations[op] = op, true
	f.operationBytes += size
	f.mu.Unlock()
	go f.execute(op)
	return nil
}

func (f *oauthFacade) execute(op *facadeOperation) {
	defer func() {
		op.cancel()
		f.mu.Lock()
		if f.operations[string(op.responseID)] == op {
			delete(f.operations, string(op.responseID))
		}
		delete(f.allOperations, op)
		delete(f.continuations, op.continuation)
		f.operationBytes -= op.bytes
		f.mu.Unlock()
	}()
	if op.modern && op.request.Method() == "subscriptions/acknowledge" {
		f.mu.Lock()
		ready := f.upstreamReady
		f.mu.Unlock()
		if ready {
			f.finish(op, f.perform(op))
			return
		}
	}
	select {
	case f.gate <- struct{}{}:
	case <-op.ctx.Done():
		f.finish(op, wire.Error(nil, -32800, "MCP request was cancelled before it could start"))
		return
	}
	release := func() {
		f.mu.Lock()
		if f.active == op {
			f.active = nil
		}
		f.mu.Unlock()
		<-f.gate
	}
	held := true
	defer func() {
		if held {
			release()
		}
	}()
	f.mu.Lock()
	f.active = op
	f.mu.Unlock()
	if err := op.ctx.Err(); err != nil {
		f.finish(op, f.operationError(op, err))
		return
	}
	if op.modern && op.request.Method() == "subscriptions/listen" {
		err := f.ensureUpstream(op)
		release()
		held = false
		if err != nil {
			f.finish(op, f.operationError(op, err))
			return
		}
		f.finish(op, f.perform(op))
		return
	}
	f.finish(op, f.perform(op))
}

func (f *oauthFacade) finish(op *facadeOperation, response wire.Message) {
	f.mu.Lock()
	send := !op.responseSent
	id := append(json.RawMessage(nil), op.responseID...)
	op.responseSent = true
	f.mu.Unlock()
	if send {
		response["id"] = id
		_ = f.send(response)
	}
}

func (f *oauthFacade) perform(op *facadeOperation) wire.Message {
	method := op.request.Method()
	if method == "initialize" {
		err := f.ensureUpstream(op)
		if err != nil {
			if f.needsLogin(op) {
				f.mu.Lock()
				f.bootstrapHandshake = true
				f.initializeComplete = true
				version := f.announcedProtocol
				f.mu.Unlock()
				return f.localInitializeResult(version)
			}
			f.mu.Lock()
			f.era = 0
			f.initializeComplete = false
			f.initializeRequest = nil
			f.mu.Unlock()
			return f.operationError(op, err)
		}
		f.mu.Lock()
		response := f.upstreamHandshake.Clone()
		var negotiated struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(response["result"], &negotiated)
		f.announcedProtocol = negotiated.ProtocolVersion
		f.initializeComplete = true
		if negotiated.ProtocolVersion != "2025-11-25" {
			f.legacyCapabilities = nil
		}
		f.mu.Unlock()
		return response
	}
	if method == "server/discover" {
		f.mu.Lock()
		ready := f.upstreamReady
		f.mu.Unlock()
		if ready {
			response, err := f.call(op, op.request)
			if err != nil {
				return f.operationError(op, err)
			}
			if response["error"] == nil || !f.needsLogin(op) {
				return response
			}
			return facadeLocalResult(nil, true, map[string]any{"supportedVersions": []string{wire.ModernProtocolVersion},
				"serverInfo": facadeServerInfo(), "capabilities": facadeCapabilities(true)})
		}
		err := f.ensureUpstream(op)
		if err != nil && !f.needsLogin(op) {
			return f.operationError(op, err)
		}
		f.mu.Lock()
		f.era = 2
		if err == nil {
			response := f.upstreamHandshake.Clone()
			f.mu.Unlock()
			return response
		}
		f.bootstrapHandshake = true
		f.mu.Unlock()
		return facadeLocalResult(nil, true, map[string]any{"supportedVersions": []string{wire.ModernProtocolVersion},
			"serverInfo": facadeServerInfo(), "capabilities": facadeCapabilities(true)})
	}
	f.mu.Lock()
	localAuthentication := method == "tools/call" && facadeToolName(op.request) == facadeAuthenticationTool && f.localToolAdvertised && !f.remoteCatalog
	op.authentication = localAuthentication
	f.mu.Unlock()
	if localAuthentication && !facadeEmptyToolArguments(op.request) {
		return wire.Error(nil, -32602, "agentcfg_authenticate accepts an empty arguments object")
	}
	if err := f.ensureUpstream(op); err != nil {
		if localAuthentication {
			return f.authenticationResult(op, err)
		}
		if f.needsLogin(op) {
			return f.blockedResult(op)
		}
		return f.operationError(op, err)
	}
	request := op.request
	if localAuthentication {
		request = facadeToolsListRequest(op.request)
	}
	response, err := f.call(op, request)
	if err != nil {
		return f.operationError(op, err)
	}
	if localAuthentication {
		if response["error"] != nil && facadeRPCCode(response) != -32601 {
			return f.authenticationResult(op, &facadeRPCError{response: response})
		}
		f.mu.Lock()
		f.remoteCatalog = true
		f.localToolAdvertised = false
		elicitation := op.elicitation
		f.mu.Unlock()
		if !op.modern {
			if elicitation != "" {
				_ = f.send(wire.Notification("notifications/elicitation/complete", map[string]any{"elicitationId": elicitation}))
			}
			_ = f.send(wire.Notification("notifications/tools/list_changed", nil))
			_ = f.send(wire.Notification("notifications/resources/list_changed", nil))
			_ = f.send(wire.Notification("notifications/prompts/list_changed", nil))
		}
		return f.authenticationResult(op, nil)
	}
	if response["error"] != nil && f.needsLogin(op) {
		return f.blockedResult(op)
	}
	if method == "tools/list" && response["error"] == nil {
		f.mu.Lock()
		f.remoteCatalog = true
		f.localToolAdvertised = false
		f.mu.Unlock()
	}
	if facadeRPCCode(response) == -32601 {
		if empty := facadeEmptyList(method); empty != nil {
			return facadeLocalResult(nil, op.modern, empty)
		}
	}
	return response
}

func (f *oauthFacade) needsLogin(op *facadeOperation) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return op.loginNeeded
}

func (f *oauthFacade) blockedResult(op *facadeOperation) wire.Message {
	f.mu.Lock()
	f.remoteCatalog = false
	if op.modern && f.era == 0 {
		f.era = 2
	}
	if op.request.Method() == "tools/list" {
		f.localToolAdvertised = true
	}
	f.mu.Unlock()
	if op.request.Method() == "tools/list" {
		return facadeLocalResult(nil, op.modern, map[string]any{"tools": []any{f.authenticationTool()}})
	}
	if empty := facadeEmptyList(op.request.Method()); empty != nil {
		return facadeLocalResult(nil, op.modern, empty)
	}
	if op.request.Method() == "tools/call" {
		return f.authenticationResult(op, auth.ErrLoginRequired)
	}
	return wire.Error(nil, -32000, f.loginMessage())
}

func (f *oauthFacade) operationError(op *facadeOperation, err error) wire.Message {
	var upstream *facadeRPCError
	if errors.As(err, &upstream) {
		return upstream.response.Clone()
	}
	if op.request.Method() == "tools/call" {
		f.mu.Lock()
		authentication := op.authentication || op.authInProgress || op.authError != nil || op.loginNeeded
		f.mu.Unlock()
		if authentication {
			return f.authenticationResult(op, err)
		}
		message := "MCP tool request failed; the operation may already have run and is not retried."
		if errors.Is(err, context.DeadlineExceeded) {
			message = "MCP tool request timed out; the operation may already have run and is not retried."
		} else if errors.Is(err, context.Canceled) {
			message = "MCP tool request was cancelled; the operation may already have run and is not retried."
		}
		return facadeLocalResult(nil, op.modern, map[string]any{"content": []any{map[string]string{"type": "text", "text": message}}, "isError": true})
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return wire.Error(nil, -32001, "MCP request timed out; the operation is not retried")
	}
	if errors.Is(err, context.Canceled) {
		return wire.Error(nil, -32800, "MCP request was cancelled")
	}
	return wire.Error(nil, -32000, "MCP upstream connection failed; retry the connection")
}
