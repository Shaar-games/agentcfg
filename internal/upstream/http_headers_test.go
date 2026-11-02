package upstream

import (
	"agentmux/internal/wire"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestModernHTTPNameAndToolParameterHeaders(t *testing.T) {
	accepted := 0
	transport := newHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("modern transport opened %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		message := decodeHTTP(t, r)
		switch message.Method() {
		case "tools/list":
			writeHTTPJSON(w, wire.Result(message.ID(), map[string]any{"tools": []any{
				map[string]any{"name": "run", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"region": map[string]any{"type": "string", "x-mcp-header": "Region"},
					"nested": map[string]any{"type": "object", "properties": map[string]any{"enabled": map[string]any{"type": "boolean", "x-mcp-header": "Enabled"}}},
					"count":  map[string]any{"type": "integer", "x-mcp-header": "Count"},
				}}},
				map[string]any{"name": "broken", "inputSchema": map[string]any{"allOf": []any{map[string]any{"x-mcp-header": "Invalid", "type": "string"}}}},
			}}))
		case "tools/call":
			if r.Header.Get("Mcp-Name") != "run" || r.Header.Get("Mcp-Param-Region") != encodeMCPHeader("Hello, 世界") ||
				r.Header.Get("Mcp-Param-Enabled") != "true" || r.Header.Get("Mcp-Param-Count") != "42" {
				t.Errorf("missing or unsafe 2026 tool parameter header: %+v", r.Header)
			}
			accepted++
			writeHTTPJSON(w, wire.Result(message.ID(), map[string]string{"ok": "yes"}))
		case "resources/read":
			if r.Header.Get("Mcp-Name") != encodeMCPHeader("file:///写真.jpg") {
				t.Errorf("unsafe resource URI header %q", r.Header.Get("Mcp-Name"))
			}
			accepted++
			writeHTTPJSON(w, wire.Result(message.ID(), map[string]any{"contents": []any{}}))
		default:
			t.Errorf("unexpected call %s", message.Method())
		}
	}))
	if err := transport.Write(modernHTTPMessage(1, "tools/list", nil)); err != nil {
		t.Fatal(err)
	}
	list := mustRead(t, transport)
	var response struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(list["result"], &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Tools) != 1 || response.Tools[0].Name != "run" {
		t.Fatalf("invalid header tool was not removed: %+v", response.Tools)
	}
	params := map[string]any{"name": "run", "arguments": map[string]any{"region": "Hello, 世界", "nested": map[string]any{"enabled": true}, "count": 42}}
	if err := transport.Write(modernHTTPMessage(2, "tools/call", params)); err != nil {
		t.Fatal(err)
	}
	if answer := mustRead(t, transport); answer["error"] != nil {
		t.Fatal(answer)
	}
	if err := transport.Write(modernHTTPMessage(3, "resources/read", map[string]any{"uri": "file:///写真.jpg"})); err != nil {
		t.Fatal(err)
	}
	if answer := mustRead(t, transport); answer["error"] != nil {
		t.Fatal(answer)
	}
	if accepted != 2 {
		t.Fatalf("handled %d calls", accepted)
	}
}

func TestHeaderSchemaVocabularyAndExactIntegers(t *testing.T) {
	for _, schema := range []any{
		map[string]any{"type": "object", "properties": map[string]any{"disabled": false, "allowed": true}},
		map[string]any{"type": "object", "examples": []any{map[string]any{"x-mcp-header": "literal"}}, "default": map[string]any{"x-mcp-header": "literal"}, "const": map[string]any{"x-mcp-header": "literal"}},
		map[string]any{"type": "object", "properties": map[string]any{"x-mcp-header": map[string]any{"type": "string"}}},
	} {
		var headers []toolParameterHeader
		if err := collectToolHeaders(schema, nil, map[string]bool{}, &headers, 0); err != nil {
			t.Fatalf("valid schema rejected: %v", err)
		}
	}
	for _, number := range []string{"42", "42.0", "4.2e1", "420e-1", "-0.0", "9007199254740991.0"} {
		if _, ok := exactHeaderInteger(json.Number(number)); !ok {
			t.Errorf("valid integer %s rejected", number)
		}
	}
	for _, number := range []string{"42.00000000000000001", "9007199254740992", "1e999999999", "1e-999999999", "0.1"} {
		if _, ok := exactHeaderInteger(json.Number(number)); ok {
			t.Errorf("invalid integer %s accepted", number)
		}
	}
}

func TestHeaderCatalogPaginationRemovalAndCallerScope(t *testing.T) {
	transport := &httpTransport{}
	makeTool := func(name, header string) any {
		return map[string]any{"name": name, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"region": map[string]any{"type": "string", "x-mcp-header": header}}}}
	}
	list := modernHTTPMessage("list", "tools/list", nil)
	capture := func(request wire.Message, tools []any, next string) {
		transport.captureToolDefinitions(wire.Result(request.ID(), map[string]any{"tools": tools, "nextCursor": next}), request)
	}
	headersFor := func(request wire.Message) http.Header {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://unused.invalid", nil)
		if err := transport.addToolParameterHeaders(req, request); err != nil {
			t.Fatal(err)
		}
		return req.Header
	}
	call := modernHTTPMessage("call", "tools/call", map[string]any{"name": "first", "arguments": map[string]any{"region": "west"}})
	capture(list, []any{makeTool("first", "First")}, "second-page")
	page2 := modernHTTPMessage("list2", "tools/list", map[string]any{"cursor": "second-page"})
	capture(page2, []any{makeTool("second", "Second")}, "")
	if headersFor(call).Get("Mcp-Param-First") != "west" {
		t.Fatal("pagination discarded first page")
	}
	other := list.Clone()
	var params map[string]json.RawMessage
	_ = json.Unmarshal(other["params"], &params)
	var meta map[string]json.RawMessage
	_ = json.Unmarshal(params["_meta"], &meta)
	meta["example.test/account"] = wire.Raw("other")
	params["_meta"] = wire.Raw(meta)
	other["params"] = wire.Raw(params)
	capture(other, []any{makeTool("first", "Other")}, "")
	if headersFor(call).Get("Mcp-Param-First") != "west" || headersFor(call).Get("Mcp-Param-Other") != "" {
		t.Fatal("caller catalogs mixed")
	}
	capture(list, []any{makeTool("second", "Second")}, "")
	if len(headersFor(call)) != 0 {
		t.Fatal("removed tool retained old annotations")
	}
	capture(list, []any{makeTool("first", "First")}, "")
	capture(list, []any{map[string]any{"name": "first", "inputSchema": map[string]any{"allOf": []any{map[string]any{"type": "string", "x-mcp-header": "Invalid"}}}}}, "")
	if len(headersFor(call)) != 0 {
		t.Fatal("invalid tool retained old annotations")
	}
}

func TestModernHeaderSentinelEncoding(t *testing.T) {
	for _, tc := range []struct {
		value   string
		encoded bool
	}{
		{"simple", false}, {"Internal Space", false}, {" whitespace ", true}, {"こんにちは", true}, {"=?base64?literal?=", true},
	} {
		got := encodeMCPHeader(tc.value)
		if strings.HasPrefix(got, "=?base64?") != tc.encoded {
			t.Errorf("%q encoded incorrectly: %q", tc.value, got)
		}
	}
}
