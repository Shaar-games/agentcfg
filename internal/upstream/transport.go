// Package upstream connects the broker to MCP servers without opening a listener.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"agentmux/internal/config"
	"agentmux/internal/wire"
)

// Transport carries raw JSON-RPC objects. Write and Close are safe to call
// concurrently. Exactly one goroutine should call Read. Close unblocks I/O.
type Transport interface {
	Read() (wire.Message, error)
	Write(wire.Message) error
	Close() error
	PID() int
}

var ErrClosed = errors.New("MCP transport is closed")

// Start uses ctx for the backend's entire lifetime, not one client request.
// stderr must return promptly from Write; it is never closed by this package.
func Start(ctx context.Context, s config.Server, stderr io.Writer) (Transport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch strings.ToLower(s.Type) {
	case "", "stdio":
		return startStdio(ctx, s, stderr)
	case "http", "streamable-http":
		return startHTTP(ctx, s)
	case "sse":
		return nil, errors.New("legacy HTTP+SSE transport is unsupported; configure a Streamable HTTP endpoint")
	default:
		return nil, fmt.Errorf("unsupported MCP transport type %q", s.Type)
	}
}
