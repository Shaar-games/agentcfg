package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agentcfg/internal/wire"
)

func facadeServerInfo() map[string]string {
	return map[string]string{"name": "agentcfg", "version": "1"}
}

func facadeCapabilities(modern bool) map[string]any {
	if modern {
		return map[string]any{"tools": map[string]any{}, "resources": map[string]any{}, "prompts": map[string]any{}}
	}
	return map[string]any{"tools": map[string]any{"listChanged": true},
		"resources": map[string]any{"listChanged": true}, "prompts": map[string]any{"listChanged": true}}
}

func facadeContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func facadeLocalResult(id json.RawMessage, modern bool, value map[string]any) wire.Message {
	if modern {
		value["resultType"] = "complete"
	}
	return wire.Result(id, value)
}

func facadeParams(message wire.Message) map[string]json.RawMessage {
	var params map[string]json.RawMessage
	_ = json.Unmarshal(message["params"], &params)
	return params
}

func facadeURLCapability(capabilities json.RawMessage) bool {
	var value struct {
		Elicitation map[string]json.RawMessage `json:"elicitation"`
	}
	if json.Unmarshal(capabilities, &value) != nil {
		return false
	}
	var mode map[string]json.RawMessage
	return json.Unmarshal(value.Elicitation["url"], &mode) == nil && mode != nil
}

func facadeModernCapabilities(message wire.Message) json.RawMessage {
	var meta map[string]json.RawMessage
	_ = json.Unmarshal(facadeParams(message)["_meta"], &meta)
	return meta["io.modelcontextprotocol/clientCapabilities"]
}

func facadeDiscoveryRequest(message wire.Message) wire.Message {
	return wire.Message{"jsonrpc": wire.Raw("2.0"), "method": wire.Raw("server/discover"),
		"params": wire.Raw(map[string]json.RawMessage{"_meta": facadeParams(message)["_meta"]})}
}

func facadeToolsListRequest(message wire.Message) wire.Message {
	params := map[string]json.RawMessage{}
	if meta := facadeParams(message)["_meta"]; meta != nil {
		params["_meta"] = meta
	}
	return wire.Message{"jsonrpc": wire.Raw("2.0"), "method": wire.Raw("tools/list"), "params": wire.Raw(params)}
}

func facadeToolName(message wire.Message) string {
	var name string
	_ = json.Unmarshal(facadeParams(message)["name"], &name)
	return name
}

func facadeValidToolCall(message wire.Message) bool {
	params := facadeParams(message)
	if params == nil || facadeToolName(message) == "" {
		return false
	}
	if arguments := params["arguments"]; arguments != nil {
		var object map[string]json.RawMessage
		if json.Unmarshal(arguments, &object) != nil || object == nil {
			return false
		}
	}
	return true
}

func facadeEmptyToolArguments(message wire.Message) bool {
	if !facadeValidToolCall(message) {
		return false
	}
	var arguments map[string]json.RawMessage
	_ = json.Unmarshal(facadeParams(message)["arguments"], &arguments)
	return len(arguments) == 0
}

func facadeCallSignature(message wire.Message) string {
	var params map[string]any
	decoder := json.NewDecoder(bytes.NewReader(message["params"]))
	decoder.UseNumber()
	if decoder.Decode(&params) != nil || params == nil {
		return ""
	}
	delete(params, "_meta")
	delete(params, "requestState")
	delete(params, "inputResponses")
	if params["arguments"] == nil && message.Method() == "tools/call" {
		params["arguments"] = map[string]any{}
	}
	data, _ := json.Marshal(params)
	return string(data)
}

func facadeRPCCode(message wire.Message) int {
	var value struct {
		Code int `json:"code"`
	}
	_ = json.Unmarshal(message["error"], &value)
	return value.Code
}

func facadeEmptyList(method string) map[string]any {
	switch method {
	case "tools/list":
		return map[string]any{"tools": []any{}}
	case "resources/list":
		return map[string]any{"resources": []any{}}
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}
	case "prompts/list":
		return map[string]any{"prompts": []any{}}
	}
	return nil
}

func (f *oauthFacade) loginMessage() string {
	// Server names are data, and quoting keeps whitespace legible in diagnostics.
	return fmt.Sprintf("OAuth login required. Call %s in a client that supports URL elicitation, or run agentcfg auth login %q, then refresh the tool list.", facadeAuthenticationTool, f.name)
}

func (f *oauthFacade) authenticationTool() map[string]any {
	return map[string]any{"name": facadeAuthenticationTool, "title": "Connect account",
		"description": fmt.Sprintf("Authorize agentcfg to access the %q MCP server. Credentials remain with agentcfg. %s", f.name, f.loginMessage()),
		"inputSchema": map[string]any{"type": "object", "additionalProperties": false},
		"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": true}}
}

func (f *oauthFacade) authenticationResult(op *facadeOperation, err error) wire.Message {
	message := "Authentication completed. Refresh the tool list to access the upstream MCP tools."
	if err != nil {
		f.mu.Lock()
		authError := op.authError
		f.mu.Unlock()
		switch {
		case errors.Is(authError, context.Canceled), errors.Is(err, context.Canceled):
			message = "Authentication was declined or cancelled; no browser fallback was started."
		case errors.Is(authError, context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
			message = "Authentication timed out. " + f.loginMessage()
		default:
			var upstream *facadeRPCError
			if errors.As(err, &upstream) && (upstream.protocol || facadeRPCCode(upstream.response) == -32601 && op.modern) {
				message = "The upstream MCP server does not support this protocol version. Reconnect the MCP client to negotiate a supported version; completed OAuth credentials remain stored by agentcfg."
			} else {
				message = f.loginMessage()
			}
		}
	}
	return facadeLocalResult(nil, op.modern, map[string]any{"content": []any{map[string]string{"type": "text", "text": message}}, "isError": err != nil})
}

func (f *oauthFacade) handleClient(message wire.Message) error {
	var jsonrpc string
	if json.Unmarshal(message["jsonrpc"], &jsonrpc) != nil || jsonrpc != "2.0" {
		return f.send(wire.Error(message.ID(), -32600, "MCP messages require jsonrpc 2.0"))
	}
	method := message.Method()
	if method == "" {
		if f.handleElicitationResponse(message) {
			return nil
		}
		return f.upstream.Write(message)
	}
	if !message.HasID() {
		switch method {
		case "notifications/initialized":
			f.mu.Lock()
			notify := false
			if f.era == 1 && f.initializeComplete {
				f.legacyReady = true
				notify = f.upstreamNeedsInitialized
				f.upstreamNeedsInitialized = false
			}
			f.mu.Unlock()
			if notify {
				return f.upstream.Write(wire.Notification("notifications/initialized", nil))
			}
		case "notifications/cancelled":
			var params struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(message["params"], &params) == nil {
				f.mu.Lock()
				op := f.operations[string(params.RequestID)]
				f.mu.Unlock()
				if op != nil && op.request.Method() != "initialize" {
					op.cancel()
				}
			}
		default:
			f.mu.Lock()
			ready := f.upstreamReady
			f.mu.Unlock()
			if ready {
				return f.upstream.Write(message)
			}
		}
		return nil
	}
	if len(message.ID()) > 512 {
		return f.send(wire.Error(nil, -32600, "MCP request identifier is too large"))
	}
	if method == "initialize" {
		return f.initializeClient(message)
	}
	modern := wire.IsModernRequest(message)
	if method == "ping" {
		return f.send(facadeLocalResult(message.ID(), modern, map[string]any{}))
	}
	f.mu.Lock()
	era, legacyReady, legacyCapabilities := f.era, f.legacyReady, f.legacyCapabilities
	f.mu.Unlock()
	if modern && era == 1 || !modern && era == 2 {
		return f.send(wire.Error(message.ID(), -32601, "MCP protocol eras cannot be mixed on this connection"))
	}
	if !modern && !legacyReady {
		return f.send(wire.Error(message.ID(), -32002, "initialize and notifications/initialized must complete before using MCP tools"))
	}
	if method == "server/discover" && !modern {
		return f.send(wire.Error(message.ID(), -32601, "server/discover requires MCP 2026 request metadata"))
	}
	if method == "tools/call" && !facadeValidToolCall(message) {
		return f.send(wire.Error(message.ID(), -32602, "tools/call requires a name and object arguments"))
	}
	if handled, err := f.resumeAuthorization(message, modern); handled {
		return err
	}
	capabilities := legacyCapabilities
	if modern {
		capabilities = facadeModernCapabilities(message)
	}
	return f.startOperation(message, modern, facadeURLCapability(capabilities))
}

func (f *oauthFacade) initializeClient(message wire.Message) error {
	var params struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
	}
	if json.Unmarshal(message["params"], &params) != nil || params.ProtocolVersion == "" || params.ProtocolVersion == wire.ModernProtocolVersion {
		return f.send(wire.Error(message.ID(), -32602, "initialize requires a supported legacy protocolVersion"))
	}
	version := params.ProtocolVersion
	if !facadeContains([]string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}, version) {
		version = "2025-11-25"
	}
	f.mu.Lock()
	if f.era != 0 {
		f.mu.Unlock()
		return f.send(wire.Error(message.ID(), -32600, "initialize already received or a different protocol is active"))
	}
	f.era, f.legacyReady = 1, false
	f.initializeComplete = false
	f.initializeRequest = message.Clone()
	f.announcedProtocol = version
	f.legacyCapabilities = append(json.RawMessage(nil), params.Capabilities...)
	if version != "2025-11-25" {
		// URL elicitation was introduced in this protocol revision.
		f.legacyCapabilities = nil
	}
	f.mu.Unlock()
	return f.startOperation(message, false, version == "2025-11-25" && facadeURLCapability(params.Capabilities))
}

func (f *oauthFacade) localInitializeResult(version string) wire.Message {
	return facadeLocalResult(nil, false, map[string]any{"protocolVersion": version,
		"capabilities": facadeCapabilities(false), "serverInfo": facadeServerInfo(),
		"instructions": strings.TrimSpace("If the upstream MCP needs OAuth, tools/list exposes agentcfg_authenticate. " + f.loginMessage())})
}

func facadeProtocolMismatchError() error {
	return &facadeRPCError{protocol: true, response: wire.Error(nil, -32601,
		"The upstream MCP protocol differs from the temporary authentication facade. Reconnect the MCP client to negotiate its supported version; completed OAuth credentials remain stored by agentcfg.")}
}
