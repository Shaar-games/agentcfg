package upstream

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"

	"agentmux/internal/wire"
)

var errRefreshToolHeaders = errors.New("refresh MCP tool parameter annotations")

// refreshToolHeaders uses the same SDK connection path as visible requests.
// It is only invoked after an explicit HeaderMismatch rejection, never after a
// timeout or an ambiguous transport failure that might have executed the tool.
func (t *httpTransport) refreshToolHeaders(ctx context.Context, original wire.Message) error {
	var originalParams struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(original["params"], &originalParams) != nil || originalParams.Meta == nil {
		return errors.New("missing MCP metadata for tool annotation refresh")
	}
	delete(originalParams.Meta, "progressToken")
	delete(originalParams.Meta, "io.modelcontextprotocol/logLevel")
	cursor := ""
	for page := 0; page < 32; page++ {
		params := map[string]any{"_meta": originalParams.Meta}
		if cursor != "" {
			params["cursor"] = cursor
		}
		request := wire.Message{
			"jsonrpc": wire.Raw("2.0"), "id": wire.Raw("agentmux-headers-" + rand.Text()),
			"method": wire.Raw("tools/list"), "params": wire.Raw(params),
		}
		var next string
		matched := false
		err := t.sdkExchange(ctx, request, func(message wire.Message) error {
			if message.Method() != "" {
				if message.HasID() {
					return errors.New("unexpected server request during tool annotation refresh")
				}
				return nil
			}
			if requestIDKey(message.ID()) != requestIDKey(request.ID()) || message["error"] != nil {
				return errors.New("invalid tool annotation refresh response")
			}
			var result struct {
				Tools      []json.RawMessage `json:"tools"`
				NextCursor string            `json:"nextCursor"`
			}
			if json.Unmarshal(message["result"], &result) != nil || result.Tools == nil {
				return errors.New("invalid tool annotation catalog")
			}
			t.captureToolDefinitions(message, request)
			matched, next = true, result.NextCursor
			return errStreamComplete
		})
		if !errors.Is(err, errStreamComplete) || !matched {
			return errors.New("MCP tool annotation refresh failed")
		}
		if next == "" {
			return nil
		}
		if next == cursor {
			return errors.New("MCP tool annotation pagination did not advance")
		}
		cursor = next
	}
	return errors.New("MCP tool annotation refresh exceeded 32 pages")
}
