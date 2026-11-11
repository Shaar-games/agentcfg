package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentcfg/internal/config"
	"agentcfg/internal/upstream"
	"agentcfg/internal/wire"
)

const maxPendingRequests = 1024
const maxCancellationReasonBytes = 1024

var nextRequest atomic.Uint64

type pending struct {
	owner         *peer
	originalID    json.RawMessage
	upstreamID    json.RawMessage
	originalToken json.RawMessage
	upstreamToken json.RawMessage
	deadline      time.Time
	reply         chan wire.Message
}

type backend struct {
	name          string
	isolated      bool
	transport     upstream.Transport
	ctx           context.Context
	cancel        context.CancelFunc
	timeout       time.Duration
	closeOnce     sync.Once
	cancellations chan wire.Message

	mu       sync.Mutex
	peers    map[*peer]bool
	pending  map[string]*pending
	progress map[string]*pending
	lastUsed time.Time

	initMu      sync.Mutex
	initResult  json.RawMessage
	protocolEra uint8 // 0 unknown, 1 legacy handshake, 2 stateless 2026; guarded by initMu
}

func newBackend(ctx context.Context, server config.Server, transport upstream.Transport, timeout time.Duration) *backend {
	ctx, cancel := context.WithCancel(ctx)
	b := &backend{
		name: server.Name, isolated: server.Sharing == "isolated", transport: transport,
		ctx: ctx, cancel: cancel, timeout: timeout,
		peers: make(map[*peer]bool), pending: make(map[string]*pending),
		progress: make(map[string]*pending), lastUsed: time.Now(),
		cancellations: make(chan wire.Message, maxPendingRequests),
	}
	go b.readLoop()
	go b.expiryLoop()
	go b.cancellationLoop()
	return b
}

func (b *backend) attach(p *peer) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx.Err() != nil {
		return fmt.Errorf("MCP backend stopped; reconnect to start a fresh process")
	}
	b.peers[p] = true
	b.lastUsed = time.Now()
	return nil
}

func (b *backend) detach(p *peer) {
	b.mu.Lock()
	delete(b.peers, p)
	b.lastUsed = time.Now()
	var cancelled []*pending
	for key, request := range b.pending {
		if request.owner == p {
			cancelled = append(cancelled, request)
			b.removeLocked(key, request)
		}
	}
	b.mu.Unlock()
	for _, request := range cancelled {
		b.cancelRequest(request, "client disconnected")
	}
	if b.isolated {
		b.close()
	}
}

func (b *backend) close() {
	b.closeOnce.Do(func() {
		b.cancel()
		b.mu.Lock()
		peers := make([]*peer, 0, len(b.peers))
		for p := range b.peers {
			peers = append(peers, p)
		}
		b.mu.Unlock()
		for _, p := range peers {
			p.close()
		}
		_ = b.transport.Close()
	})
}

func (b *backend) handle(p *peer, message wire.Message) {
	if b.isolated {
		if err := b.transport.Write(message); err != nil {
			p.close()
		}
		return
	}
	method := message.Method()
	if method == "initialize" && message.HasID() {
		if !p.initialized.CompareAndSwap(0, 1) {
			p.send(wire.Error(message.ID(), -32600, "initialize already received on this connection"))
			return
		}
		go b.initialize(p, message)
		return
	}
	if method == "server/discover" && message.HasID() {
		if p.initialized.CompareAndSwap(0, 1) {
			go b.discover(p, message)
		} else if p.initialized.Load() == 2 {
			if _, err := b.forward(p, message, nil); err != nil {
				p.send(wire.Error(message.ID(), -32000, err.Error()))
			}
		} else {
			p.send(wire.Error(message.ID(), -32600, "MCP discovery already pending"))
		}
		return
	}
	if method == "ping" && message.HasID() {
		p.send(wire.Result(message.ID(), map[string]any{}))
		return
	}
	if method == "notifications/initialized" || method == "notifications/roots/list_changed" {
		return
	}
	if p.initialized.Load() == 0 && wire.IsModernRequest(message) {
		b.initMu.Lock()
		if b.protocolEra == 1 {
			b.initMu.Unlock()
			p.send(wire.Error(message.ID(), -32601, "upstream uses legacy MCP; reconnect with initialize or configure sharing=isolated"))
			return
		}
		b.protocolEra = 2
		p.initialized.Store(2)
		b.initMu.Unlock()
	}
	if p.initialized.Load() != 2 {
		if message.HasID() {
			p.send(wire.Error(message.ID(), -32002, "initialize must complete before using a shared MCP server"))
		}
		return
	}
	if method == "notifications/cancelled" {
		b.forwardCancellation(p, message)
		return
	}
	if method == "" {
		return // No server-to-client requests are advertised in shared mode.
	}
	if method == "resources/subscribe" || method == "resources/unsubscribe" || method == "logging/setLevel" || method == "subscriptions/listen" || method == "subscriptions/acknowledge" {
		if message.HasID() {
			p.send(wire.Error(message.ID(), -32601, "client-specific subscriptions or state are unavailable in shared mode; configure sharing=isolated"))
		}
		return
	}
	if strings.HasPrefix(method, "tasks/") || hasTaskRequest(message) {
		if message.HasID() {
			p.send(wire.Error(message.ID(), -32601, "task lifecycle is session-bound; configure sharing=isolated"))
		}
		return
	}
	if !message.HasID() {
		if err := b.transport.Write(message); err != nil {
			b.close()
		}
		return
	}
	if _, err := b.forward(p, message, nil); err != nil {
		p.send(wire.Error(message.ID(), -32000, err.Error()))
	}
}

func (b *backend) initialize(p *peer, message wire.Message) {
	b.initMu.Lock()
	defer b.initMu.Unlock()
	if b.protocolEra == 2 {
		p.initialized.Store(0)
		p.send(wire.Error(message.ID(), -32601, "upstream uses stateless MCP 2026; configure sharing=isolated for legacy clients"))
		return
	}
	if b.initResult == nil {
		result, err := b.initializeUpstream(message)
		if err != nil {
			p.initialized.Store(0)
			p.send(wire.Error(message.ID(), -32000, err.Error()))
			return
		}
		if result["error"] != nil {
			result["id"] = message.ID()
			p.initialized.Store(0)
			p.send(result)
			return
		}
		b.initResult = sanitizeCapabilities(result["result"])
		b.protocolEra = 1
		if err := b.transport.Write(wire.Notification("notifications/initialized", nil)); err != nil {
			b.close()
			return
		}
	}
	p.initialized.Store(2)
	p.send(wire.Message{"jsonrpc": wire.Raw("2.0"), "id": message.ID(), "result": b.initResult})
}

// discover forwards the modern, request-scoped handshake alternative. A modern
// server may be shared among modern peers without initializing a global client
// session; unsupported discovery leaves the connection eligible for legacy
// initialize fallback. The two protocol eras cannot share one stdio connection.
func (b *backend) discover(p *peer, message wire.Message) {
	b.initMu.Lock()
	defer b.initMu.Unlock()
	if b.protocolEra == 1 {
		p.initialized.Store(0)
		p.send(wire.Error(message.ID(), -32601, "server/discover unavailable after legacy MCP initialization"))
		return
	}
	replies := make(chan wire.Message, 1)
	request, err := b.forward(nil, message, replies)
	if err != nil {
		p.initialized.Store(0)
		p.send(wire.Error(message.ID(), -32000, err.Error()))
		return
	}
	limit := b.timeout
	if limit > 5*time.Second {
		limit = 5 * time.Second
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	var response wire.Message
	select {
	case response = <-replies:
	case <-timer.C:
		b.mu.Lock()
		b.removeLocked(string(request.upstreamID), request)
		b.mu.Unlock()
		if cancellable, ok := b.transport.(interface{ CancelRequest(json.RawMessage) }); ok {
			// Drain an abandoned HTTP discovery before allowing the legacy
			// initialize fallback on this same upstream transport.
			cancellable.CancelRequest(request.upstreamID)
		}
		p.initialized.Store(0)
		p.send(wire.Error(message.ID(), -32001, "modern MCP discovery timed out; legacy initialization may be available"))
		return
	case <-b.ctx.Done():
		p.initialized.Store(0)
		p.send(wire.Error(message.ID(), -32000, "MCP stopped during discovery"))
		return
	}
	response["id"] = message.ID()
	if response["error"] != nil {
		p.initialized.Store(0)
		p.send(response)
		return
	}
	var result struct {
		SupportedVersions []string `json:"supportedVersions"`
	}
	if json.Unmarshal(response["result"], &result) != nil {
		p.initialized.Store(0)
		p.send(wire.Error(message.ID(), -32600, "invalid MCP discovery response"))
		return
	}
	supported := false
	for _, v := range result.SupportedVersions {
		if v == wire.ModernProtocolVersion {
			supported = true
			break
		}
	}
	if !supported {
		p.initialized.Store(0)
		p.send(wire.Error(message.ID(), -32601, "upstream does not advertise MCP 2026-07-28"))
		return
	}
	b.protocolEra = 2
	p.initialized.Store(2)
	p.send(response)
}

func (b *backend) initializeUpstream(message wire.Message) (wire.Message, error) {
	request := message.Clone()
	var params map[string]json.RawMessage
	if err := json.Unmarshal(request["params"], &params); err != nil || params == nil {
		return nil, fmt.Errorf("initialize requires an object params")
	}
	var protocol string
	if err := json.Unmarshal(params["protocolVersion"], &protocol); err != nil || protocol == "" {
		return nil, fmt.Errorf("initialize requires protocolVersion")
	}
	// A shared upstream has no single client to own roots, sampling or elicitation.
	params["capabilities"] = wire.Raw(map[string]any{})
	params["clientInfo"] = wire.Raw(map[string]string{"name": "agentcfg", "version": "0.1.0"})
	request["params"] = wire.Raw(params)
	reply := make(chan wire.Message, 1)
	if _, err := b.forward(nil, request, reply); err != nil {
		return nil, err
	}
	select {
	case result := <-reply:
		return result, nil
	case <-b.ctx.Done():
		return nil, fmt.Errorf("MCP stopped during initialization")
	}
}

func sanitizeCapabilities(result json.RawMessage) json.RawMessage {
	var document map[string]json.RawMessage
	if json.Unmarshal(result, &document) != nil || document == nil {
		return result
	}
	var capabilities map[string]json.RawMessage
	if json.Unmarshal(document["capabilities"], &capabilities) != nil || capabilities == nil {
		return result
	}
	delete(capabilities, "logging")
	delete(capabilities, "tasks")
	var resources map[string]json.RawMessage
	if json.Unmarshal(capabilities["resources"], &resources) == nil && resources != nil {
		delete(resources, "subscribe")
		capabilities["resources"] = wire.Raw(resources)
	}
	document["capabilities"] = wire.Raw(capabilities)
	return wire.Raw(document)
}

func hasTaskRequest(message wire.Message) bool {
	var params map[string]json.RawMessage
	return json.Unmarshal(message["params"], &params) == nil && params["task"] != nil
}

func (b *backend) forward(owner *peer, message wire.Message, reply chan wire.Message) (*pending, error) {
	request := &pending{owner: owner, originalID: append(json.RawMessage(nil), message.ID()...),
		upstreamID: wire.Raw(fmt.Sprintf("agentcfg-%d", nextRequest.Add(1))),
		deadline:   time.Now().Add(b.timeout), reply: reply}
	forwarded := message.Clone()
	forwarded["id"] = request.upstreamID
	remapProgress(forwarded, request)
	key := string(request.upstreamID)
	b.mu.Lock()
	if len(b.pending) >= maxPendingRequests {
		b.mu.Unlock()
		return nil, fmt.Errorf("shared MCP has reached its limit of %d pending requests", maxPendingRequests)
	}
	for _, existing := range b.pending {
		if owner != nil && existing.owner == owner && string(existing.originalID) == string(message.ID()) {
			b.mu.Unlock()
			return nil, fmt.Errorf("request id is already pending on this connection")
		}
	}
	b.pending[key] = request
	if request.upstreamToken != nil {
		b.progress[string(request.upstreamToken)] = request
	}
	b.mu.Unlock()
	if err := b.transport.Write(forwarded); err != nil {
		b.mu.Lock()
		b.removeLocked(key, request)
		b.mu.Unlock()
		return nil, fmt.Errorf("MCP write failed: %w", err)
	}
	return request, nil
}

func remapProgress(message wire.Message, request *pending) {
	var params, meta map[string]json.RawMessage
	if json.Unmarshal(message["params"], &params) != nil || params == nil {
		return
	}
	if json.Unmarshal(params["_meta"], &meta) != nil || meta == nil || meta["progressToken"] == nil {
		return
	}
	request.originalToken = meta["progressToken"]
	request.upstreamToken = wire.Raw(fmt.Sprintf("agentcfg-progress-%d", nextRequest.Add(1)))
	meta["progressToken"] = request.upstreamToken
	params["_meta"] = wire.Raw(meta)
	message["params"] = wire.Raw(params)
}

func (b *backend) removeLocked(key string, request *pending) {
	delete(b.pending, key)
	if request.upstreamToken != nil {
		delete(b.progress, string(request.upstreamToken))
	}
}

func (b *backend) readLoop() {
	defer b.close()
	for {
		var message wire.Message
		var parentID json.RawMessage
		var err error
		if transport, ok := b.transport.(interface {
			ReadRouted() (wire.Message, json.RawMessage, error)
		}); ok {
			message, parentID, err = transport.ReadRouted()
		} else {
			message, err = b.transport.Read()
		}
		if err != nil {
			return
		}
		b.routeFrom(message, parentID)
	}
}

func (b *backend) route(message wire.Message) {
	b.routeFrom(message, nil)
}

// parentID is the upstream ID of the client request whose HTTP POST carried
// this message. For a stateless MCP, the response stream is the correlation
// boundary for notifications that don't contain their own request ID.
func (b *backend) routeFrom(message wire.Message, parentID json.RawMessage) {
	if b.isolated {
		b.broadcast(message)
		return
	}
	if method := message.Method(); method != "" {
		if message.HasID() {
			response := wire.Error(message.ID(), -32601, "agentcfg shared mode has no owning client; use sharing=isolated for client callbacks")
			if method == "ping" {
				response = wire.Result(message.ID(), map[string]any{})
			}
			_ = b.transport.Write(response)
		} else if method == "notifications/progress" {
			b.routeProgress(message)
		} else if method == "notifications/tools/list_changed" || method == "notifications/resources/list_changed" || method == "notifications/prompts/list_changed" {
			// These are global list invalidations, safe to share across peers.
			b.broadcast(message)
		} else if len(parentID) != 0 {
			b.mu.Lock()
			request := b.pending[string(parentID)]
			b.mu.Unlock()
			if request != nil && request.owner != nil {
				request.owner.send(message)
			}
		} else {
			// Modern subscription notifications and request-scoped diagnostic
			// details have no verified owner here. Broadcasting them would leak
			// one harness's state to all others. Isolated mode preserves them.
		}
		return
	}
	key := string(message.ID())
	b.mu.Lock()
	request := b.pending[key]
	if request != nil {
		b.removeLocked(key, request)
	}
	b.mu.Unlock()
	if request != nil {
		message["id"] = request.originalID
		b.deliver(request, message)
	}
}

func (b *backend) deliver(request *pending, message wire.Message) {
	if request.reply != nil {
		select {
		case request.reply <- message:
		default:
		}
	} else if request.owner != nil {
		request.owner.send(message)
	}
}

func (b *backend) routeProgress(message wire.Message) {
	var params map[string]json.RawMessage
	if json.Unmarshal(message["params"], &params) != nil {
		return
	}
	b.mu.Lock()
	request := b.progress[string(params["progressToken"])]
	b.mu.Unlock()
	if request == nil || request.owner == nil {
		return
	}
	params["progressToken"] = request.originalToken
	message["params"] = wire.Raw(params)
	request.owner.send(message)
}

func (b *backend) broadcast(message wire.Message) {
	b.mu.Lock()
	peers := make([]*peer, 0, len(b.peers))
	for p := range b.peers {
		if b.isolated || p.initialized.Load() == 2 {
			peers = append(peers, p)
		}
	}
	b.mu.Unlock()
	for _, p := range peers {
		p.send(message)
	}
}

func (b *backend) forwardCancellation(p *peer, message wire.Message) {
	var params map[string]json.RawMessage
	if json.Unmarshal(message["params"], &params) != nil {
		return
	}
	b.mu.Lock()
	var match *pending
	for _, request := range b.pending {
		if request.owner == p && string(request.originalID) == string(params["requestId"]) {
			match = request
			break
		}
	}
	b.mu.Unlock()
	if match != nil {
		var reason string
		if raw := params["reason"]; raw != nil && json.Unmarshal(raw, &reason) != nil {
			return
		}
		b.cancelRequest(match, reason)
	}
}

func (b *backend) cancelRequest(request *pending, reason string) {
	// Cancellation reasons are advisory. Retaining arbitrary client frames in
	// the bounded queue could still retain gigabytes when a writer is stalled.
	// Preserve only the protocol fields and a small, valid UTF-8 reason.
	if len(reason) > maxCancellationReasonBytes {
		reason = strings.ToValidUTF8(reason[:maxCancellationReasonBytes], "")
	}
	b.queueCancellation(wire.Notification("notifications/cancelled", map[string]any{
		"requestId": request.upstreamID, "reason": reason,
	}))
}

// An upstream that stops reading stdin must not stop deadline delivery, peer
// cleanup or the caller's read loop while an advisory cancellation is written.
// One bounded queue avoids a goroutine per cancellation. If the upstream cannot
// drain it, stop this backend so clients can reconnect to a fresh process.
func (b *backend) queueCancellation(message wire.Message) {
	if b.ctx.Err() != nil {
		return
	}
	select {
	case <-b.ctx.Done():
	case b.cancellations <- message.Clone():
	default:
		b.close()
	}
}

func (b *backend) cancellationLoop() {
	for {
		select {
		case <-b.ctx.Done():
			return
		case message := <-b.cancellations:
			if b.ctx.Err() != nil {
				return
			}
			if err := b.transport.Write(message); err != nil {
				if errors.Is(err, upstream.ErrBusy) {
					// Drop this advisory notification when transport capacity is
					// temporarily full; other requests and shared clients stay live.
					continue
				}
				b.close()
				return
			}
		}
	}
}

func (b *backend) expiryLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case now := <-ticker.C:
			b.expire(now)
		}
	}
}

func (b *backend) expire(now time.Time) {
	b.mu.Lock()
	var expired []*pending
	for key, request := range b.pending {
		if now.After(request.deadline) {
			expired = append(expired, request)
			b.removeLocked(key, request)
		}
	}
	b.mu.Unlock()
	for _, request := range expired {
		b.deliver(request, wire.Error(request.originalID, -32001, "MCP request timed out; the operation may already have run and is not retried"))
		b.cancelRequest(request, "agentcfg request timeout")
	}
}
