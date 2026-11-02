// Package upstream adapts the official MCP Go SDK v1.8.0 to the broker's
// Read/Write/Close/PID interface.
//
// Stdio uses mcp.IOTransport with the existing process-tree ownership. HTTP
// POSTs use mcp.StreamableClientTransport and the OAuthHandler supplied by
// internal/auth. Raw connections preserve MCP params, results, error.data,
// per-request metadata, MRTR continuations and task methods without substituting
// a typed ClientSession or implementing server features on the client's behalf.
// An HTTP connection per POST retains the origin needed by ReadRouted.
//
// The SDK's private sessionUpdated and Tool-context hooks are unavailable to a
// raw proxy. A bounded legacy GET adapter therefore handles unsolicited legacy
// messages using the SDK JSON-RPC decoder; HTTP decorators retain legacy session
// state, parameter annotations and safe header encoding. Modern requests never
// establish a session or issue GET/DELETE. Stream resumption is not supported.
// Only SDK OAuth authorization and an explicit HeaderMismatch rejection can
// cause a POST retry; ambiguous execution failures are not replayed.
//
// The raw SDK is not a fully transparent arbitrary JSON-RPC codec: it discards
// unknown root envelope/error members and converts numeric IDs through float64
// and int64. Outbound unsupported members and IDs outside exact safe integers
// are rejected, instead of being silently changed. Incoming envelopes follow
// the SDK's codec behavior; arbitrary root extensions and fractional numeric
// server-callback IDs cannot be preserved. The broker uses string request IDs.
// MCP extensions belong in payload metadata, which stays raw. JSON-RPC batches
// are rejected because the broker's message API has no batch representation.
//
// Frames are bounded at 32 MiB. Eight ordinary HTTP requests use two-minute
// contexts; eight subscriptions have their own capacity and live until cancelled
// or the backend closes. Four control slots remain available to legacy callbacks.
// Tool annotations are scoped to caller metadata, with at most 64 cached
// namespaces and 32 pages per refresh. No runtime HTTP listener is created here.
package upstream
