package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/wire"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func (f *oauthFacade) fetchAuthorization(ctx context.Context, redirect string, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
	f.mu.Lock()
	op := f.active
	available := op != nil && op.ctx.Err() == nil &&
		((op.urlSupported && op.allowUI) || (!op.urlSupported && f.autoBrowser && f.opts.AuthorizationCodeFetcher != nil))
	if !available && op != nil {
		op.loginNeeded = true
		op.authError = auth.ErrLoginRequired
	}
	if available {
		op.authInProgress = true
	}
	f.mu.Unlock()
	if !available {
		// An idle or unsupported client must let the shared broker try another
		// connected client. This is distinct from a user's decline or cancellation.
		return nil, ErrAuthorizationUnavailable
	}
	defer func() {
		f.mu.Lock()
		op.authInProgress = false
		f.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	stop := context.AfterFunc(op.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := op.ctx.Err(); err != nil {
		return nil, err
	}
	var result *sdkauth.AuthorizationResult
	var err error
	if op.urlSupported {
		result, err = f.opts.ClientAuthorizationCodeFetcher(ctx, redirect, args, func(presentationContext context.Context, target string) error {
			if args == nil || target != args.URL || !facadeSafeAuthorizationURL(target) {
				return errors.New("invalid OAuth authorization URL")
			}
			return f.presentAuthorization(presentationContext, op, target)
		})
	} else {
		result, err = f.opts.AuthorizationCodeFetcher(ctx, redirect, args)
	}
	f.mu.Lock()
	op.loginNeeded = err != nil
	op.authError = err
	f.mu.Unlock()
	return result, err
}

func facadeSafeAuthorizationURL(target string) bool {
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if u.Scheme != "https" {
		address, err := netip.ParseAddr(u.Hostname())
		loopback := strings.EqualFold(u.Hostname(), "localhost") || err == nil && address.IsLoopback()
		if u.Scheme != "http" || !loopback {
			return false
		}
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for name := range query {
		switch strings.ToLower(name) {
		case "code", "access_token", "refresh_token", "client_secret":
			return false
		}
	}
	return true
}

func (f *oauthFacade) presentAuthorization(ctx context.Context, op *facadeOperation, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !op.allowUI || !op.urlSupported || op.request.Method() != "tools/call" {
		return ErrAuthorizationUnavailable
	}
	params := map[string]any{"mode": "url", "url": target,
		"message": "Authorize agentcfg to connect to the configured MCP service. Credentials remain with agentcfg."}
	if op.modern {
		return f.presentModernAuthorization(ctx, op, params)
	}
	id := facadeOpaqueID("agentcfg-elicitation-")
	params["elicitationId"] = id
	reply := make(chan wire.Message, 1)
	f.mu.Lock()
	op.elicitation = id
	f.elicitationReplies[string(wire.Raw(id))] = reply
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.elicitationReplies, string(wire.Raw(id)))
		f.mu.Unlock()
	}()
	if err := f.send(wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(id),
		"method": wire.Raw("elicitation/create"), "params": wire.Raw(params)}); err != nil {
		return err
	}
	select {
	case response := <-reply:
		if response["error"] != nil {
			return errors.New("MCP client could not present OAuth authorization")
		}
		return facadeElicitationAction(response["result"])
	case <-ctx.Done():
		return ctx.Err()
	case <-op.ctx.Done():
		return op.ctx.Err()
	}
}

func facadeElicitationAction(raw json.RawMessage) error {
	var response struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return errors.New("invalid OAuth elicitation response")
	}
	switch response.Action {
	case "accept":
		return nil
	case "decline", "cancel":
		return context.Canceled
	default:
		return errors.New("invalid OAuth elicitation action")
	}
}

func (f *oauthFacade) handleElicitationResponse(message wire.Message) bool {
	f.mu.Lock()
	reply := f.elicitationReplies[string(message.ID())]
	if reply != nil {
		delete(f.elicitationReplies, string(message.ID()))
	}
	f.mu.Unlock()
	if reply == nil {
		return false
	}
	select {
	case reply <- message:
	default:
	}
	return true
}

func (f *oauthFacade) presentModernAuthorization(ctx context.Context, op *facadeOperation, params map[string]any) error {
	state := facadeOpaqueID("agentcfg-auth-")
	f.mu.Lock()
	op.continuation = state
	f.continuations[state] = op
	id := append(json.RawMessage(nil), op.responseID...)
	op.responseSent = true
	if f.operations[string(id)] == op {
		delete(f.operations, string(id))
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.continuations, state)
		f.mu.Unlock()
	}()
	response := wire.Result(id, map[string]any{"resultType": "input_required", "requestState": state,
		"inputRequests": map[string]any{"oauth": map[string]any{"method": "elicitation/create", "params": params}}})
	if err := f.send(response); err != nil {
		return err
	}
	select {
	case message := <-op.resume:
		var inputs map[string]json.RawMessage
		_ = json.Unmarshal(facadeParams(message)["inputResponses"], &inputs)
		return facadeElicitationAction(inputs["oauth"])
	case <-ctx.Done():
		return ctx.Err()
	case <-op.ctx.Done():
		return op.ctx.Err()
	}
}

func (f *oauthFacade) resumeAuthorization(message wire.Message, modern bool) (bool, error) {
	if message.Method() != "tools/call" {
		return false, nil
	}
	params := facadeParams(message)
	var state string
	_ = json.Unmarshal(params["requestState"], &state)
	if state == "" {
		return false, nil
	}
	f.mu.Lock()
	op := f.continuations[state]
	if op == nil {
		local := f.localToolAdvertised && !f.remoteCatalog && facadeToolName(message) == facadeAuthenticationTool
		f.mu.Unlock()
		if local {
			return true, f.send(wire.Error(message.ID(), -32602, "OAuth continuation is unknown or expired; start a new authentication call"))
		}
		return false, nil
	}
	var inputs map[string]json.RawMessage
	validInputs := json.Unmarshal(params["inputResponses"], &inputs) == nil && len(inputs) == 1 && inputs["oauth"] != nil
	actionErr := facadeElicitationAction(inputs["oauth"])
	validAction := actionErr == nil || errors.Is(actionErr, context.Canceled)
	if !modern || op.ctx.Err() != nil || !validInputs || !validAction || facadeCallSignature(message) != op.signature ||
		string(message.ID()) == string(op.responseID) || string(message.ID()) == string(op.originalID) || f.operations[string(message.ID())] != nil {
		f.mu.Unlock()
		return true, f.send(wire.Error(message.ID(), -32602, "OAuth continuation requires the same tool arguments, a new request ID and one valid URL elicitation response"))
	}
	delete(f.continuations, state)
	op.responseID = append(json.RawMessage(nil), message.ID()...)
	op.responseSent = false
	f.operations[string(message.ID())] = op
	f.mu.Unlock()
	select {
	case op.resume <- message.Clone():
		return true, nil
	case <-op.ctx.Done():
		return true, f.send(wire.Error(message.ID(), -32800, "OAuth continuation was cancelled"))
	}
}
