package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/wire"
)

// Transport carries raw JSON-RPC objects. Write and Close are safe to call
// concurrently. Exactly one goroutine should call Read. Close unblocks I/O.
type Transport interface {
	Read() (wire.Message, error)
	Write(wire.Message) error
	Close() error
	PID() int
}

// RoutedTransport preserves the HTTP request that produced an incoming message.
// The parent ID is nil for an unsolicited legacy GET event. Callers must choose
// either Read or ReadRouted, using exactly one reader for the transport.
type RoutedTransport interface {
	Transport
	ReadRouted() (wire.Message, json.RawMessage, error)
}

var ErrClosed = errors.New("MCP transport is closed")

// Start uses ctx for the backend's entire lifetime, not one client request.
// stderr must return promptly from Write; it is never closed by this package.
func Start(ctx context.Context, s config.Server, stderr io.Writer) (Transport, error) {
	return start(ctx, s, stderr, nil)
}

// StartWithOAuth enables browser authorization supplied by the local client.
// The HTTP SDK owns discovery, token use and retries after an auth rejection.
// Stdio upstreams retain their own authentication behavior unchanged.
func StartWithOAuth(ctx context.Context, s config.Server, stderr io.Writer, opts auth.Options) (Transport, error) {
	return start(ctx, s, stderr, &opts)
}

func start(ctx context.Context, s config.Server, stderr io.Writer, opts *auth.Options) (Transport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch strings.ToLower(s.Type) {
	case "", "stdio":
		return startStdio(ctx, s, stderr)
	case "http", "streamable-http":
		return startHTTPWithOAuth(ctx, s, opts)
	case "sse":
		return nil, errors.New("legacy HTTP+SSE transport is unsupported; configure a Streamable HTTP endpoint")
	default:
		return nil, fmt.Errorf("unsupported MCP transport type %q", s.Type)
	}
}
