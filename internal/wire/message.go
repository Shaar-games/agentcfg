// Package wire provides bounded newline-delimited JSON-RPC framing.
package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

const MaxFrameSize = 32 << 20

// ModernProtocolVersion is the request-scoped MCP revision introduced in 2026.
// It has no initialize handshake or transport-level HTTP session.
const ModernProtocolVersion = "2026-07-28"

// Message keeps unknown MCP fields intact when request IDs are rewritten.
type Message map[string]json.RawMessage

func Raw(value any) json.RawMessage {
	b, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("wire.Raw: %v", err))
	}
	return b
}

func (m Message) ID() json.RawMessage { return m["id"] }

func (m Message) Method() string {
	var method string
	_ = json.Unmarshal(m["method"], &method)
	return method
}

func (m Message) Clone() Message {
	copy := make(Message, len(m))
	for key, value := range m {
		copy[key] = append(json.RawMessage(nil), value...)
	}
	return copy
}

func (m Message) HasID() bool { return len(m.ID()) != 0 && !bytes.Equal(m.ID(), []byte("null")) }

// IsModernRequest identifies a well-formed stateless 2026 request envelope.
// Preserve the original _meta when forwarding: it belongs to the originating
// client, not to the shared broker process.
func IsModernRequest(m Message) bool {
	if m.Method() == "" || !m.HasID() {
		return false
	}
	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(m["params"], &params) != nil || params.Meta == nil {
		return false
	}
	var version string
	if json.Unmarshal(params.Meta["io.modelcontextprotocol/protocolVersion"], &version) != nil || version != ModernProtocolVersion {
		return false
	}
	var capabilities map[string]json.RawMessage
	return json.Unmarshal(params.Meta["io.modelcontextprotocol/clientCapabilities"], &capabilities) == nil && capabilities != nil
}

func Error(id json.RawMessage, code int, message string) Message {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return Message{"jsonrpc": Raw("2.0"), "id": id, "error": Raw(map[string]any{"code": code, "message": message})}
}

func Result(id json.RawMessage, value any) Message {
	return Message{"jsonrpc": Raw("2.0"), "id": id, "result": Raw(value)}
}

func Notification(method string, params any) Message {
	m := Message{"jsonrpc": Raw("2.0"), "method": Raw(method)}
	if params != nil {
		m["params"] = Raw(params)
	}
	return m
}

func Decode(data []byte) (Message, error) {
	if len(data) > MaxFrameSize {
		return nil, errors.New("JSON-RPC frame exceeds 32 MiB")
	}
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC object: %w", err)
	}
	if m == nil {
		return nil, errors.New("JSON-RPC message must be an object")
	}
	return m, nil
}

type Reader struct{ scanner *bufio.Scanner }

func NewReader(reader io.Reader) *Reader {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), MaxFrameSize+1)
	return &Reader{scanner: scanner}
}

func (r *Reader) Read() (Message, error) {
	for r.scanner.Scan() {
		data := bytes.TrimSpace(r.scanner.Bytes())
		if len(data) == 0 {
			continue
		}
		return Decode(data)
	}
	if err := r.scanner.Err(); err != nil {
		return nil, fmt.Errorf("read JSON-RPC frame (maximum 32 MiB): %w", err)
	}
	return nil, io.EOF
}

type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

func NewWriter(writer io.Writer) *Writer { return &Writer{w: writer} }

func (w *Writer) Write(m Message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(data) > MaxFrameSize {
		return errors.New("JSON-RPC frame exceeds 32 MiB")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	data = append(data, '\n')
	for len(data) > 0 {
		n, err := w.w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
