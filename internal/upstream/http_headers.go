package upstream

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"agentmux/internal/wire"
)

// Tool header annotations use direct JSON Schema properties paths only.
// When a schema is malformed, we omit the invalid tool from tools/list as
// required by the MCP 2026 Streamable HTTP transport specification.
type toolParameterHeader struct {
	header string
	path   []string
	kind   string
}

func annotationsIn(value any, depth int) bool {
	if depth > 32 {
		return true
	}
	switch item := value.(type) {
	case map[string]any:
		if _, present := item["x-mcp-header"]; present {
			return true
		}
		for key, child := range item {
			if schemaAnnotationIn(key, child, depth+1) {
				return true
			}
		}
	}
	return false
}

// JSON Schema annotations apply to schema nodes, not literal example/default
// data or property names. Only vocabulary positions containing schemas recurse.
func schemaAnnotationIn(keyword string, value any, depth int) bool {
	switch keyword {
	case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies":
		if object, ok := value.(map[string]any); ok {
			for _, schema := range object {
				if annotationsIn(schema, depth+1) {
					return true
				}
			}
		}
	case "allOf", "anyOf", "oneOf", "prefixItems":
		if array, ok := value.([]any); ok {
			for _, schema := range array {
				if annotationsIn(schema, depth+1) {
					return true
				}
			}
		}
	case "additionalProperties", "unevaluatedProperties", "propertyNames", "contains",
		"items", "additionalItems", "unevaluatedItems", "not", "if", "then", "else":
		if array, ok := value.([]any); ok {
			for _, schema := range array {
				if annotationsIn(schema, depth+1) {
					return true
				}
			}
		} else {
			return annotationsIn(value, depth+1)
		}
	}
	return false
}

func collectToolHeaders(value any, path []string, used map[string]bool, output *[]toolParameterHeader, depth int) error {
	if depth > 16 {
		return errors.New("tool schema nesting exceeds 16 levels")
	}
	if _, ok := value.(bool); ok && len(path) != 0 {
		return nil
	}
	schema, ok := value.(map[string]any)
	if !ok {
		return errors.New("tool schema is not an object")
	}
	if raw, present := schema["x-mcp-header"]; present {
		header, ok := raw.(string)
		if !ok || !validHeaderName(header) || len(path) == 0 {
			return errors.New("invalid x-mcp-header name or location")
		}
		kind, _ := schema["type"].(string)
		if kind != "string" && kind != "integer" && kind != "boolean" {
			return errors.New("x-mcp-header must annotate a string, integer or boolean property")
		}
		lowered := strings.ToLower(header)
		if used[lowered] {
			return errors.New("duplicate x-mcp-header")
		}
		used[lowered] = true
		*output = append(*output, toolParameterHeader{header: header, path: append([]string(nil), path...), kind: kind})
	}
	for key, child := range schema {
		switch key {
		case "x-mcp-header":
			continue
		case "properties":
			properties, ok := child.(map[string]any)
			if !ok {
				return errors.New("schema.properties must be an object")
			}
			for name, nested := range properties {
				if err := collectToolHeaders(nested, append(append([]string(nil), path...), name), used, output, depth+1); err != nil {
					return fmt.Errorf("property %s: %w", name, err)
				}
			}
		default:
			if schemaAnnotationIn(key, child, 0) {
				return fmt.Errorf("x-mcp-header must not occur beneath %s", key)
			}
		}
	}
	return nil
}

type toolHeaderCatalog struct{ nextCursor string }

// Header definitions can differ with per-request capabilities or extension
// metadata. Do not mix catalogs from distinct callers; transient progress/log
// settings do not alter the namespace used for a hidden refresh.
func toolHeaderScope(request wire.Message) string {
	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	_ = json.Unmarshal(request["params"], &params)
	delete(params.Meta, "progressToken")
	delete(params.Meta, "io.modelcontextprotocol/logLevel")
	data, _ := json.Marshal(params.Meta)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) + "\x00"
}

func (t *httpTransport) captureToolDefinitions(message wire.Message, source ...wire.Message) {
	if message["result"] == nil {
		return
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(message["result"], &result) != nil || result == nil {
		return
	}
	var tools []json.RawMessage
	if json.Unmarshal(result["tools"], &tools) != nil {
		return
	}
	filtered := make([]json.RawMessage, 0, len(tools))
	updates := make(map[string][]toolParameterHeader)
	invalid := make(map[string]bool)
	for _, raw := range tools {
		var tool struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		}
		if json.Unmarshal(raw, &tool) != nil || tool.Name == "" {
			continue
		}
		invalid[tool.Name] = true
		var schema any
		if json.Unmarshal(tool.InputSchema, &schema) != nil {
			continue
		}
		headers := []toolParameterHeader{}
		if err := collectToolHeaders(schema, nil, map[string]bool{}, &headers, 0); err != nil {
			// An invalid tool must not be exposed on a modern HTTP transport.
			continue
		}
		updates[tool.Name] = headers
		delete(invalid, tool.Name)
		filtered = append(filtered, raw)
	}
	var request wire.Message
	if len(source) != 0 {
		request = source[0]
	}
	scope := toolHeaderScope(request)
	var params struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(request["params"], &params)
	var nextCursor string
	_ = json.Unmarshal(result["nextCursor"], &nextCursor)
	t.mu.Lock()
	if t.toolHeaders == nil {
		t.toolHeaders = map[string][]toolParameterHeader{}
	}
	if t.toolCatalogs == nil {
		t.toolCatalogs = map[string]*toolHeaderCatalog{}
	}
	if t.toolCatalogs[scope] == nil && len(t.toolCatalogs) >= 64 {
		// This is a cache, not an authorization store. Bound namespace growth
		// from caller metadata; an evicted annotation is refreshed on rejection.
		clear(t.toolCatalogs)
		clear(t.toolHeaders)
	}
	catalog := t.toolCatalogs[scope]
	if params.Cursor == "" || catalog == nil || catalog.nextCursor != params.Cursor {
		for key := range t.toolHeaders {
			if strings.HasPrefix(key, scope) {
				delete(t.toolHeaders, key)
			}
		}
	}
	t.toolCatalogs[scope] = &toolHeaderCatalog{nextCursor: nextCursor}
	for name := range invalid {
		delete(t.toolHeaders, scope+name)
	}
	for name, headers := range updates {
		t.toolHeaders[scope+name] = headers
	}
	t.mu.Unlock()
	result["tools"] = wire.Raw(filtered)
	message["result"] = wire.Raw(result)
}

func readParameter(arguments map[string]any, path []string) (any, bool) {
	var value any = arguments
	for _, name := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		next, found := object[name]
		if !found {
			return nil, false
		}
		value = next
	}
	if value == nil {
		return nil, false
	}
	return value, true
}

func (t *httpTransport) addToolParameterHeaders(req *http.Request, rpc wire.Message) error {
	if rpc.Method() != "tools/call" {
		return nil
	}
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(rpc["params"], &params) != nil {
		return errors.New("invalid modern tool request parameters")
	}
	t.mu.Lock()
	mappings := append([]toolParameterHeader(nil), t.toolHeaders[toolHeaderScope(rpc)+params.Name]...)
	t.mu.Unlock()
	if len(mappings) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(params.Arguments))
	decoder.UseNumber()
	var arguments map[string]any
	if err := decoder.Decode(&arguments); err != nil {
		return errors.New("invalid modern tool arguments")
	}
	for _, mapping := range mappings {
		raw, found := readParameter(arguments, mapping.path)
		if !found {
			continue
		}
		var value string
		switch mapping.kind {
		case "string":
			parsed, ok := raw.(string)
			if !ok {
				return fmt.Errorf("tool argument for %s must be string", mapping.header)
			}
			value = parsed
		case "boolean":
			parsed, ok := raw.(bool)
			if !ok {
				return fmt.Errorf("tool argument for %s must be boolean", mapping.header)
			}
			value = strconv.FormatBool(parsed)
		case "integer":
			parsed, ok := raw.(json.Number)
			if !ok {
				return fmt.Errorf("tool argument for %s must be integer", mapping.header)
			}
			number, ok := exactHeaderInteger(parsed)
			if !ok {
				return fmt.Errorf("tool argument for %s exceeds safe integer range", mapping.header)
			}
			value = strconv.FormatInt(number, 10)
		}
		req.Header.Set("Mcp-Param-"+mapping.header, encodeMCPHeader(value))
	}
	return nil
}

func exactHeaderInteger(number json.Number) (int64, bool) {
	// Reuse lossless decimal normalization. Floating point conversion would
	// wrongly accept 42.00000000000000001; big.Rat could allocate enormous
	// integers for an attacker-controlled exponent such as 1e999999999.
	canonical := strings.TrimPrefix(numberIDKey(number.String()), "number:")
	if canonical == "0" {
		return 0, true
	}
	digits, exponentText, hasExponent := strings.Cut(canonical, "e")
	exponent, err := strconv.ParseInt(exponentText, 10, 64)
	if !hasExponent || err != nil || exponent < 0 || exponent > 15 ||
		int64(len(strings.TrimPrefix(digits, "-")))+exponent > 16 {
		return 0, false
	}
	value, err := strconv.ParseInt(digits+strings.Repeat("0", int(exponent)), 10, 64)
	return value, err == nil && value >= -9007199254740991 && value <= 9007199254740991
}
