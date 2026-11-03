package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"agentcfg/internal/wire"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

var errSDKEnvelope = errors.New("the MCP SDK does not support extension members outside params, result or error.data")
var errSDKIdentifier = errors.New("the MCP SDK requires string IDs or integers within the exact JSON number range")

// Params, results, error data and their extension metadata stay raw. The SDK's
// Extra field is application state, not wire data; reject unsupported envelopes
// rather than silently dropping outbound extension fields.
func decodeSDKMessage(message wire.Message) (jsonrpc.Message, error) {
	if err := validateSDKEnvelope(message); err != nil {
		return nil, err
	}
	data, err := json.Marshal(message)
	if err != nil || len(data) > wire.MaxFrameSize || !utf8.Valid(data) {
		return nil, errors.New("invalid MCP message or frame exceeds 32 MiB")
	}
	decoded, err := jsonrpc.DecodeMessage(data)
	if err != nil {
		return nil, errors.New("MCP message is not supported by the SDK JSON-RPC codec")
	}
	return decoded, nil
}

func encodeSDKMessage(message jsonrpc.Message) (wire.Message, error) {
	data, err := jsonrpc.EncodeMessage(message)
	if err != nil || len(data) > wire.MaxFrameSize || !utf8.Valid(data) {
		return nil, errors.New("invalid SDK MCP message or frame exceeds 32 MiB")
	}
	decoded, err := wire.Decode(data)
	if err != nil {
		return nil, err
	}
	if !sdkIdentifierSupported(decoded.ID()) {
		return nil, errSDKIdentifier
	}
	return decoded, nil
}

func validateSDKEnvelope(message wire.Message) error {
	for key := range message {
		switch key {
		case "jsonrpc", "id", "method", "params", "result", "error":
		default:
			return errSDKEnvelope
		}
	}
	if message["error"] != nil {
		var object map[string]json.RawMessage
		if json.Unmarshal(message["error"], &object) == nil {
			for key := range object {
				if key != "code" && key != "message" && key != "data" {
					return errSDKEnvelope
				}
			}
		}
	}
	if !sdkIdentifierSupported(message.ID()) {
		return errSDKIdentifier
	}
	return nil
}

func sdkIdentifierSupported(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return false
	}
	switch value := value.(type) {
	case string:
		return true
	case json.Number:
		_, valid := exactHeaderInteger(value)
		return valid
	default:
		return false
	}
}

// A raw SDK IO connection does not receive the private protocol-version hook
// that disables pre-2025 JSON-RPC batches. The broker API carries one message
// per frame, so reject a batch before it can be split or regrouped by the SDK.
type singleMessageReader struct {
	io.ReadCloser
	atStart bool
	err     error
}

func (r *singleMessageReader) Read(data []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.ReadCloser.Read(data)
	for index, value := range data[:n] {
		if value == '\n' {
			r.atStart = true
		} else if r.atStart && value != ' ' && value != '\t' && value != '\r' {
			r.atStart = false
			if value == '[' {
				r.err = errors.New("MCP JSON-RPC batches are unsupported")
				if index == 0 {
					return 0, r.err
				}
				return index, nil
			}
		}
	}
	return n, err
}
