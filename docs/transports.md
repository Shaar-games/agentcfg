# Upstream transports

`internal/upstream.Start(ctx, server, stderr)` opens one upstream connection for
the backend lifetime. It does not initialize MCP itself: the broker sends
`initialize` and `notifications/initialized`, decides sharing policy and routes
request IDs, callbacks and progress notifications.

```go
type Transport interface {
    Read() (wire.Message, error)
    Write(wire.Message) error
    Close() error
    PID() int
}
```

Use one reader. Concurrent writes and repeated/concurrent closes are supported.
Closing cancels outstanding I/O and waits for the transport's workers. A supplied
stderr writer must return promptly from `Write`; arbitrary blocking application
writers cannot be interrupted through the `io.Writer` interface. The transport
never closes that supplied writer. `PID()` is the actual direct process PID for
stdio and zero for HTTP.

## Stdio

The executable and argument array are separate. Native executables are launched
directly, with no shell expression evaluation. The resolved working directory
and a deterministic merge of the inherited environment and server overrides are
used for execution and executable lookup. Windows environment names are matched
case-insensitively; if override spellings collide, the lexically last spelling
wins. No implicit current-directory executable search is added: name lookup uses
the configured `PATH` and Windows `PATHEXT`. Paths containing spaces are supported.
The same executable lookup and MCP transport serve every configured stdio command.

On Windows, a command without an extension is resolved through `PATHEXT`, so
the POSIX `npx` shim shipped beside `npx.cmd` is not selected. A name containing
a dot also tries the configured suffixes after its exact spelling. Empty Windows
`PATH` components are ignored; an explicit `.` still requests the working
directory. Unix keeps the usual empty-component working-directory behavior.

If a canonical stdio server omits `cwd`, the connection supplies its caller's
working directory over private IPC. The broker resolves that directory before
selecting an upstream process, so its own launch directory cannot leak into a
later client's context. An explicit server `cwd` takes precedence. Clients with
the same resolved configuration and directory can share; different directories
produce different backend identities. No project-local canonical files are read
or written to establish this runtime context.

On Unix, the child's `PWD` follows its effective working directory as well,
unless the server explicitly overrides `PWD` in its environment.

When the **server configuration** supplies a nonempty `PLUGIN_DATA`, execution
creates that directory and, if needed, a selected working directory beneath it.
Configuration discovery does not create directories. Merely inheriting
`PLUGIN_DATA` from the broker environment does not create it. A missing working
directory outside the configured data directory is an error.

Windows processes start suspended and hidden. Only private duplicates of the
three child pipe handles are inherited. A kill-on-close Job Object is configured
and the suspended process is attached before its primary thread is resumed. If
job creation, restriction, assignment or resumption fails, startup fails and any
created process is terminated and waited for. The job does not grant child
breakaway permission. Closing the job terminates its descendants, including
children started by `.cmd` wrappers. The parent process handle is always waited
for and released.

On Unix, the process starts in a new process group. Close sends `SIGKILL` to that
group and waits for the direct child. This covers descendants that remain in the
group; a program deliberately creating a separate session/group can escape that
mechanism. It is process cleanup, not an operating-system sandbox.

If the direct process exits naturally, remaining group/job members are stopped,
but buffered stdout remains readable. This preserves the last JSON-RPC response.
Each stdio message is one UTF-8 JSON-RPC object terminated by LF; CRLF is also
accepted. The payload, including surrounding spaces or tabs, is at most
**32 MiB**, excluding the delimiter. The reader validates the complete line
before exposing it to the SDK, so pipe read boundaries cannot change which
messages are accepted. Adjacent objects without a delimiter, batches, embedded
physical newlines, invalid UTF-8, oversized input and an unterminated final line
fail the read. Earlier valid lines remain readable before that error. Child
stderr stays separate from protocol stdout. Delimiter scanning visits each
received byte once, including when large messages arrive in small pipe reads.

### Windows batch wrappers

`.cmd` and `.bat` require the system `cmd.exe`. The transport uses an explicit
system interpreter, disables AutoRun and delayed expansion, and quotes the
resolved script and every argument. This supports ordinary `npx.cmd` style
wrappers and script paths/arguments containing spaces, ampersands, pipes,
parentheses and carets without treating those argument characters as commands.

Batch arguments containing a double quote, `%`, `!`, NUL or a line break are
rejected before launch. Arguments ending in a backslash are also rejected
because cmd and an executable called through `%*` interpret that quoting
differently. A batch script path containing those unsafe characters is rejected.
For these values, configure the underlying executable directly, such as
`node.exe` plus the JavaScript entry point and its argument array. Native
executables retain native argument quoting, including quotes, percent signs,
exclamation marks and trailing backslashes. The transport does not make the
contents of an arbitrary batch script safe; it prevents configuration arguments
from breaking its own command invocation.

## Outbound Streamable HTTP

`type: "streamable-http"` selects this transport. The URL
must be one absolute HTTP(S) MCP endpoint without a fragment. Literal URL
credentials and query parameters are preserved. All POST, GET and DELETE requests
use that endpoint. Redirects are rejected, including
same-origin redirects; authentication headers are never forwarded to a redirect
target. No TCP/HTTP listener is opened by this implementation. Local test servers
exist only inside `httptest` fixtures.

Configured headers are copied for each request. MCP content, session and protocol
headers are managed by the transport; configuration cannot override the host or
connection framing. HTTP session IDs are distinct from JSON-RPC request IDs.

Each outgoing message becomes one POST. Requests accept either one JSON response
or an SSE stream that may contain notifications and server callbacks before the
matching response. Callback responses and notifications accept HTTP 202 or 204.
Errors for outgoing requests are delivered by `Read` as JSON-RPC errors with the original
request ID. Failed notifications/responses terminate the transport because there
is no request ID to which an error could be returned. Transport-generated errors
omit URLs, configured headers, response bodies and underlying network error text.
HTTP rejections retain their numeric status and distinguish authentication,
access denial, redirects, rate limits and server errors. Typed DNS, certificate,
connection, timeout and OAuth token/authorization failures use fixed messages.
An HTTP 401 alone cannot distinguish missing credentials from rejected ones.
Client connections can perform OAuth discovery and browser sign-in when an
upstream authentication challenge requires it; HTTP status alone does not
authorize replaying an operation after an unrelated network failure.
Invalid or interrupted JSON/SSE responses are reported without copying their
contents. Raw valid MCP messages, including server-supplied error responses,
retain their content for the broker.

Initialization response headers are retained before parsing its SSE events, so
callbacks during initialization can use the session. The negotiated protocol
version and successful session state are installed before exposing the initialize
result. Later POSTs and the optional GET stream wait for the initialized
notification's HTTP acceptance. Subsequent requests include `Mcp-Session-Id`, when
assigned, and `MCP-Protocol-Version`. A session-bearing HTTP 404 ends the transport;
reconnection must start a fresh initialization. Network failures and session
expiration do not trigger replay of tool requests.

A shared initialization stays owned by the backend if its first client
disconnects. Other clients can use its actual result; a new handshake only starts
after a failed result has reset the transport. MCP does not permit cancellation
of `initialize`. If its deadline expires, the backend closes so a subsequent
connection cannot reuse an unfinished session.

After initialization, one optional GET SSE stream receives unsolicited requests
and notifications. HTTP 405/501 or an unsupported GET response disables that
optional channel while POST remains available. A clean GET EOF reconnects after
one second. GET streams rotate after five minutes and are cancelled at close.
Malformed GET streams and other GET failures disable the optional channel, except
session expiration or a decoded message that cannot be delivered before its
deadline: those failures end the session instead of silently dropping that data.

The SSE reader handles LF, CRLF and bare CR, a leading UTF-8 BOM, comments,
multiline `data` fields, empty priming events and events split across reads. It
requires a complete event delimiter; an incomplete event at EOF is discarded.
Both a line and the total encoded event are bounded by 32 MiB, including framing
overhead. JSON responses and outgoing messages are also bounded by 32 MiB.
Request-ID matching preserves number/string distinctions, arbitrary integer
precision and equivalent string escapes; the original encoding is used when
delivering the correlated response to the broker.

### Bounds, cancellation and shutdown

- At most eight ordinary request POSTs, eight modern `subscriptions/listen` POSTs
  and four separate control/reply POSTs are active.
  No goroutine is started before a slot is reserved. A saturated lane makes
  `Write` return `ErrBusy` immediately. The broker must handle that write error.
- Separate control capacity lets callback replies and cancellation proceed even
  when all request slots hold streams awaiting a reply.
- In shared mode, an advisory cancellation rejected with `ErrBusy` is dropped
  without disconnecting attached clients. Exhausting the broker's bounded
  cancellation queue or encountering a fatal write error still stops the backend.
- The receive queue contains at most eight messages. Backpressure observes the
  operation context. If a failed/cancelled request's completion cannot be queued,
  the transport terminates so the operation does not leave a blocked worker.
- All POSTs except modern `subscriptions/listen` have a two-minute overall
  deadline, including response headers and body. Response headers still have a
  two-minute timeout for subscriptions. The dial and TLS handshake deadlines are
  ten seconds. Backend context cancellation interrupts all active operations.
- In a legacy session, `notifications/cancelled` for an active cancellable request
  is forwarded to the server and cancels the matching local POST. Initialization,
  unknown IDs and completed requests ignore cancellation, including a notification
  arriving after failed discovery has reset the transport. Modern cancellation
  closes the matching POST stream without another notification POST. Local
  cancellation is not proof that the remote operation stopped; it may already
  have executed.
- Close cancels active operations, waits for workers, attempts session DELETE
  with a fresh context bounded to two seconds, and closes idle connections.
  DELETE errors and HTTP 405 are best effort and do not prevent local cleanup.

### Modern MCP 2026-07-28 (stateless)

In addition to the 2025 initialization/session protocol, the transport accepts
requests carrying the `2026-07-28` version and capabilities inside `params._meta`.
It also supports the optional `server/discover` request. Modern requests do not
use `initialize`, `notifications/initialized`, `Mcp-Session-Id`, the HTTP GET
stream, or a session DELETE. `MCP-Protocol-Version` and `Mcp-Method` are set
for each request. `Mcp-Name` is set for `tools/call`, `prompts/get`, and
`resources/read`, using `params.name` or `params.uri` as appropriate.

The broker waits for an abandoned discovery to finish cancelling before allowing
legacy initialization on the same HTTP transport, including when its client
disconnects or its request deadline expires.

Header values requiring escaping are represented as `=?base64?...?=` following
the protocol's sentinel encoding. The transport caches validated
`x-mcp-header` parameter annotations from modern `tools/list` responses
(including nested direct properties) and mirrors applicable tool arguments into
`Mcp-Param-*` headers. Invalid annotated tool schemas are excluded from the
modern list. A direct `tools/call` **before** `tools/list` might lack schema-
derived headers; clients should list tools first when using annotated tools.

If a modern `tools/call` receives `HeaderMismatch` (`-32020`), the transport
refreshes `tools/list` and, if that succeeds, retries the call once with updated
headers. This error is a protocol rejection before execution; other tool failures
do not trigger an automatic replay.

Modern requests are routed independently and retain the originating client's
`_meta`. Two clients' identical JSON-RPC IDs are remapped by the broker; modern
clients are not merged into an already-initialized legacy stdio session.
Normal modern JSON/SSE results, including multi-round-trip `input_required`
results, are forwarded as received. MRTR input collection remains the calling
client's responsibility. Modern `subscriptions/listen` streams omit the ordinary
overall request deadline. They are not automatically restarted or resumed with
an SSE cursor.

### Explicit transport boundaries

The legacy initialization/session/JSON/SSE path targets 2025 Streamable HTTP,
including 2025-03-26/2025-06-18 and ordinary 2025-11-25 streams. It does **not**
implement SSE cursor resumption, `Last-Event-ID`
or the 2025-11-25 polling behavior where a server closes a POST stream before its
result and expects GET resumption. Such an incomplete POST stream returns a
correlated error; the operation is not replayed. Unsolicited GET reconnection is
not lossless and does not replay missed events.

OAuth 2.1 authorization uses the official Go MCP SDK. When a client starts
`agentcfg connect`, a required OAuth sign-in opens the browser from that client
process, including for a remote HTTPS entry containing only its URL. The local MCP
connection stays on stdio. Authorization prompts and callback responses travel
over private broker IPC and never become MCP messages or consume the client's
stdin. Browser instructions go only to stderr. Credentials remain in agentcfg's
encrypted store; native client token caches are not imported.

Concurrent clients with the same credential identity share one sign-in. If its
client disconnects, another connected client can finish the same authorization.
Cancellation waits for callback cleanup before a new flow can reuse its port.
After upgrading, restart an existing broker with `agentcfg stop` before
reconnecting clients: this private IPC capability requires the updated broker.

Discovery, PKCE, encrypted cached tokens and refresh are supported. An explicit
`oauth` block can select a preregistered client, scopes, account or callback.
It also uses saved credentials before the first resource challenge, so a server
that permits anonymous access can still return the authenticated user's results.
Storage errors remain visible instead of silently downgrading to anonymous access.
Explicit scopes limit discovery defaults. Scopes required by a Bearer challenge
are included in the next authorization, together with existing grants.
A callback issuer is still validated when the provider omits its metadata hint.
Dynamic registration uses the SDK's redirect classification, including HTTPS
loopback callbacks as native applications.

Automatic browser login requires an HTTP loopback callback with a nonzero port.
The callback listener is bound before the browser opens and is closed on success,
failure, cancellation or timeout. A configured redirect URL must be registered
with the authorization provider; configurations requiring a manual callback can
still use `agentcfg auth login <server>`. That command also supports `--no-browser`.
No terminal input is requested by `connect`. A client-imposed startup or request
deadline can also cancel an in-progress browser sign-in.

Windows uses per-user DPAPI, while Linux/macOS need an explicitly supplied
`AGENTCFG_OAUTH_KEY` (base64-encoded 32 random bytes) or an injected secure
store. Public URL-only entries remain independent of credential storage until an
OAuth challenge requires it. Static configured headers are supported without OAuth; a configured
literal Authorization header cannot be combined with OAuth. HTTP 401/403
failures without valid credentials return sanitized errors. Legacy 2024
HTTP+SSE (`type: "sse"`, with
a separate server-selected POST endpoint) is explicitly unsupported. There is no
implicit fallback to that protocol and no endpoint supplied by SSE is followed.

## Verification

The default package tests use the actual `go test` executable as a subprocess
fixture, temporary files/directories and `httptest` endpoints. They do not modify
installed harness configurations, contact an external MCP or depend on Node.js.
They cover process identity, native/batch quoting, environment/cwd,
data-directory creation, tree cleanup, concurrent close/write, cancellation,
malformed/oversized frames, initialize/session/header ordering, callback replies,
optional GET, JSON/SSE results, session expiration, redirect rejection and
sanitized correlated errors.

Browser callback tests use an injected opener instead of launching a real browser.
They verify listener availability before navigation, callback state/path/method,
provider refusal, output failures, cancellation and port cleanup without terminal
input. The OAuth integration opens a transport before login, verifies the unauthenticated
error, then performs PKCE login and encrypted credential persistence against an
SDK test server. It checks an authenticated tool call, rejection after logout and
recovery after another login on that same transport, for legacy and stateless
protocols with JSON and SSE responses.

`AGENTCFG_TEST_NPX=1` explicitly enables the real npm integration. It downloads
`@modelcontextprotocol/server-filesystem@2.2.0` into a temporary cache and requires
Node.js 24 or newer. `TestNPXFilesystem` checks the transport directly;
`TestCLINPXFilesystem` additionally checks the CLI, broker and shared/isolated
clients. See [verification.md](verification.md) for the disposable Docker runner.

```text
mise exec -- go test ./internal/upstream -count=1
mise exec -- go test -race ./internal/upstream -count=1
mise exec -- go vet ./internal/upstream
```

Protocol references:

- [MCP 2025-06-18 transports](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)
- [MCP 2026-07-28 stdio](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/stdio)
- [MCP 2026-07-28 Streamable HTTP](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)
- [MCP 2025-11-25 transports](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
- [MCP lifecycle](https://modelcontextprotocol.io/specification/2025-06-18/basic/lifecycle)
- [MCP cancellation](https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/cancellation)
- [SSE parsing](https://html.spec.whatwg.org/multipage/server-sent-events.html#parsing-an-event-stream)
