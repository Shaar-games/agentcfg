package upstream

import (
	"bufio"
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

// Validate a complete physical line before exposing any of it to the SDK's
// streaming JSON decoder. Otherwise pipe read boundaries can make adjacent
// objects, batches, or multiline objects look like separate valid messages.
type singleMessageReader struct {
	io.ReadCloser
	scanner *bufio.Scanner
	pending []byte
	err     error
}

func (r *singleMessageReader) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	if r.scanner == nil {
		r.scanner = bufio.NewScanner(r.ReadCloser)
		// The payload limit excludes its LF or CRLF delimiter.
		r.scanner.Buffer(make([]byte, 64<<10), wire.MaxFrameSize+2)
		r.scanner.Split(splitStdioLine)
	}
	for len(r.pending) == 0 {
		if !r.scanner.Scan() {
			r.err = r.scanner.Err()
			if r.err == nil {
				r.err = io.EOF
			}
			return 0, r.err
		}
		line := r.scanner.Bytes()
		if len(line) > wire.MaxFrameSize {
			r.err = errors.New("MCP stdio frame exceeds 32 MiB")
			return 0, r.err
		}
		message := bytes.Trim(line, " \t")
		if len(message) == 0 {
			continue
		}
		if message[0] == '[' {
			r.err = errors.New("MCP JSON-RPC batches are unsupported")
			return 0, r.err
		}
		if message[0] != '{' || bytes.ContainsRune(line, '\r') || !utf8.Valid(line) || !json.Valid(message) {
			r.err = errors.New("MCP stdio line must contain one complete UTF-8 JSON-RPC object")
			return 0, r.err
		}
		// Retain this scanner token until fully delivered. Only framing
		// whitespace is normalized; the JSON object remains unchanged.
		r.pending = append(message, '\n')
	}
	n := copy(data, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func splitStdioLine(data []byte, atEOF bool) (int, []byte, error) {
	if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
		line := data[:newline]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		return newline + 1, line, nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return 0, nil, nil
}
