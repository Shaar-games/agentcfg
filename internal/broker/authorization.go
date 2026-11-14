package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// Private envelopes separate broker control from untrusted MCP messages. An
// upstream server cannot turn an MCP payload into a local browser instruction.
// Leave room for the envelope around a complete maximum-size MCP message.
const maxIPCFrameSize = wire.MaxFrameSize + 1024

var errAuthorizationClientClosed = errors.New("OAuth client disconnected")

// ErrAuthorizationUnavailable lets another attached client present the same
// grant when this client has no eligible operation or URL presentation support.
// It is not a refusal: a user's decline or cancellation remains terminal.
var ErrAuthorizationUnavailable = errors.New("OAuth client cannot present authorization for this operation")

// ConnectOptions belongs to the local stdio proxy, never the remote MCP server.
// AuthorizationCodeFetcher must honor cancellation and must not use MCP stdin
// or stdout. The broker keeps token exchange and credential storage private.
type ConnectOptions struct {
	AuthorizationCodeFetcher func(context.Context, string, *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error)
	// ClientAuthorizationCodeFetcher binds the local callback before presenting
	// the authorization URL through the MCP client's declared URL capability.
	// The presenter never receives the authorization code or resulting tokens.
	ClientAuthorizationCodeFetcher func(context.Context, string, *sdkauth.AuthorizationArgs, func(context.Context, string) error) (*sdkauth.AuthorizationResult, error)
}

type authorizationRequest struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	RedirectURL string `json:"redirectUrl"`
}

type authorizationResponse struct {
	ID     string                       `json:"id"`
	Result *sdkauth.AuthorizationResult `json:"result,omitempty"`
	Error  string                       `json:"error,omitempty"`
}

type authorizationRelay struct {
	redirect string
	mu       sync.Mutex
	peers    map[*peer]bool
}

func newAuthorizationRelay(redirect string) *authorizationRelay {
	return &authorizationRelay{redirect: redirect, peers: make(map[*peer]bool)}
}

func (r *authorizationRelay) attach(p *peer) {
	if !p.authorizes {
		return
	}
	r.mu.Lock()
	r.peers[p] = true
	r.mu.Unlock()
}

func (r *authorizationRelay) detach(p *peer) {
	r.mu.Lock()
	delete(r.peers, p)
	r.mu.Unlock()
}

func (r *authorizationRelay) fetch(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
	if args == nil || args.URL == "" || len(args.URL) > 16384 || r.redirect == "" {
		return nil, errors.New("invalid OAuth browser request")
	}
	attempted := make(map[*peer]bool)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.mu.Lock()
		var selected *peer
		for p := range r.peers {
			if attempted[p] {
				continue
			}
			select {
			case <-p.done:
				continue
			default:
			}
			if selected == nil || p.id < selected.id {
				selected = p
			}
		}
		r.mu.Unlock()
		if selected == nil {
			return nil, auth.ErrLoginRequired
		}
		attempted[selected] = true
		result, err := selected.authorize(ctx, r.redirect, args.URL)
		if !errors.Is(err, errAuthorizationClientClosed) && !errors.Is(err, ErrAuthorizationUnavailable) {
			return result, err
		}
		// Another connected client for this credential identity may finish the
		// same grant. A user cancellation or provider failure is never retried.
	}
}

var nextAuthorization atomic.Uint64

func (p *peer) authorize(ctx context.Context, redirect, url string) (*sdkauth.AuthorizationResult, error) {
	id := fmt.Sprintf("oauth-%d", nextAuthorization.Add(1))
	reply := make(chan authorizationResponse, 1)
	p.authMu.Lock()
	if p.authPending == nil {
		p.authPending = make(map[string]chan authorizationResponse)
	}
	p.authPending[id] = reply
	p.authMu.Unlock()
	defer func() {
		p.authMu.Lock()
		delete(p.authPending, id)
		p.authMu.Unlock()
	}()
	p.sendFrame(wire.Message{"authorize": wire.Raw(authorizationRequest{ID: id, URL: url, RedirectURL: redirect})})
	select {
	case response := <-reply:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch response.Error {
		case "":
			if response.Result == nil {
				return nil, errors.New("OAuth browser returned no authorization response")
			}
			return response.Result, nil
		case "cancelled":
			return nil, context.Canceled
		case "timeout":
			return nil, context.DeadlineExceeded
		case "disconnected":
			return nil, errAuthorizationClientClosed
		case "unavailable":
			return nil, ErrAuthorizationUnavailable
		case "login_required":
			return nil, auth.ErrLoginRequired
		default:
			return nil, errors.New("OAuth browser authorization failed; inspect the MCP client diagnostics")
		}
	case <-ctx.Done():
		p.sendFrame(wire.Message{"cancelAuthorization": wire.Raw(id)})
		// A new grant uses the same callback address. Keep this flight alive
		// until the local process confirms its listener has actually closed.
		// The cancelled caller already returns through the flight coordinator.
		cleanup := time.NewTimer(5 * time.Second)
		defer cleanup.Stop()
		select {
		case <-reply:
		case <-p.done:
		case <-cleanup.C:
			p.close()
		}
		return nil, ctx.Err()
	case <-p.done:
		return nil, errAuthorizationClientClosed
	}
}

func (p *peer) authorizationResponse(raw json.RawMessage) error {
	var response authorizationResponse
	if len(raw) > 32768 || json.Unmarshal(raw, &response) != nil || response.ID == "" || len(response.ID) > 128 {
		return errors.New("invalid OAuth IPC response")
	}
	p.authMu.Lock()
	reply := p.authPending[response.ID]
	p.authMu.Unlock()
	if reply != nil {
		select {
		case reply <- response:
		default:
		}
	}
	return nil // A cancelled flow can still have a response in transit.
}

func decodeMCPEnvelope(message wire.Message) (wire.Message, error) {
	if len(message) != 1 || message["mcp"] == nil {
		return nil, errors.New("invalid agentcfg MCP envelope")
	}
	return wire.Decode(message["mcp"])
}

func encodeMCPEnvelope(message wire.Message) (wire.Message, error) {
	raw, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	if len(raw) > wire.MaxFrameSize {
		return nil, errors.New("JSON-RPC frame exceeds 32 MiB")
	}
	return wire.Message{"mcp": raw}, nil
}

type clientAuthorization struct {
	ctx      context.Context
	redirect string
	writer   *wire.Writer
	opts     ConnectOptions
	mu       sync.Mutex
	flows    map[string]context.CancelFunc
	wg       sync.WaitGroup
	closed   bool
}

func (c *clientAuthorization) handle(message wire.Message) error {
	if len(message) != 1 {
		return errors.New("invalid agentcfg authorization envelope")
	}
	if raw := message["cancelAuthorization"]; raw != nil {
		var id string
		if json.Unmarshal(raw, &id) != nil || id == "" || len(id) > 128 {
			return errors.New("invalid OAuth cancellation")
		}
		c.mu.Lock()
		cancel := c.flows[id]
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	}
	var request authorizationRequest
	if message["authorize"] == nil || len(message["authorize"]) > 32768 || json.Unmarshal(message["authorize"], &request) != nil ||
		request.ID == "" || len(request.ID) > 128 || request.URL == "" || len(request.URL) > 16384 ||
		request.RedirectURL != c.redirect || c.redirect == "" {
		return errors.New("invalid OAuth browser request from broker")
	}
	c.mu.Lock()
	if c.closed || c.ctx.Err() != nil {
		c.mu.Unlock()
		return context.Canceled
	}
	if _, exists := c.flows[request.ID]; exists || len(c.flows) != 0 {
		c.mu.Unlock()
		return errors.New("duplicate or concurrent OAuth browser request")
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Minute)
	c.flows[request.ID] = cancel
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer cancel()
		result, err := c.opts.AuthorizationCodeFetcher(ctx, request.RedirectURL, &sdkauth.AuthorizationArgs{URL: request.URL})
		response := authorizationResponse{ID: request.ID, Result: result}
		if err != nil || result == nil {
			response.Result = nil
			switch {
			case errors.Is(err, context.Canceled):
				response.Error = "cancelled"
			case errors.Is(err, context.DeadlineExceeded):
				response.Error = "timeout"
			case errors.Is(err, auth.ErrLoginRequired):
				response.Error = "login_required"
			case errors.Is(err, ErrAuthorizationUnavailable):
				response.Error = "unavailable"
			default:
				response.Error = "failed"
			}
		}
		if c.ctx.Err() != nil {
			// The fetcher has returned and released its callback listener. This
			// acknowledgement allows another live client to take over safely.
			response.Result = nil
			response.Error = "disconnected"
		}
		// Raw callback errors can contain authorization codes. Only a fixed
		// classification crosses IPC; the local UI owns its diagnostics.
		c.mu.Lock()
		delete(c.flows, request.ID)
		c.mu.Unlock()
		_ = c.writer.Write(wire.Message{"authorization": wire.Raw(response)})
	}()
	return nil
}

func (c *clientAuthorization) close() {
	c.mu.Lock()
	c.closed = true
	for _, cancel := range c.flows {
		cancel()
	}
	c.mu.Unlock()
	c.wg.Wait()
}
