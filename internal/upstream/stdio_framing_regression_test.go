package upstream

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/wire"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stdioFramingChunks fixes the boundaries returned by the underlying reader.
// This avoids depending on the operating system's pipe buffering.
type stdioFramingChunks struct {
	chunks []string
}

func (r *stdioFramingChunks) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.chunks) > 0 {
		if r.chunks[0] == "" {
			r.chunks = r.chunks[1:]
			continue
		}
		n := copy(p, r.chunks[0])
		r.chunks[0] = r.chunks[0][n:]
		return n, nil
	}
	return 0, io.EOF
}

func readStdioFramingFixture(t *testing.T, chunks []string) ([]wire.Message, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reader := &singleMessageReader{
		ReadCloser: io.NopCloser(&stdioFramingChunks{chunks: append([]string(nil), chunks...)}),
	}
	connection, err := (&mcp.IOTransport{
		Reader: reader, Writer: discardWriteCloser{io.Discard}, MaxLineLength: wire.MaxFrameSize + 2,
	}).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	var messages []wire.Message
	for {
		message, err := connection.Read(ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("stdio fixture blocked after its input was exhausted")
			}
			return messages, err
		}
		encoded, err := encodeSDKMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, encoded)
		if len(messages) > 4 {
			t.Fatal("stdio fixture produced more messages than its input objects")
		}
	}
}

func TestStdioFramingRejectsAdjacentValuesWithoutNewline(t *testing.T) {
	// MCP requires one complete JSON-RPC message per line, regardless of how
	// the byte stream is split across reads:
	// https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/stdio
	const first = "{\"jsonrpc\":\"2.0\",\"id\":\"first\",\"result\":true}"
	const second = "{\"jsonrpc\":\"2.0\",\"id\":\"second\",\"result\":false}"
	for _, test := range []struct {
		name   string
		chunks []string
	}{
		{"objects-one-read", []string{first + second + "\n"}},
		{"objects-split-before-second", []string{first, second + "\n"}},
		{"batch-one-read", []string{first + "[" + second + "]\n"}},
		{"batch-split-before-array", []string{first, "[" + second + "]\n"}},
		{"embedded-newline-one-read", []string{strings.ReplaceAll(first, ",", ",\n") + "\n"}},
		{"embedded-newline-split", []string{"{", "\n", first[1:] + "\n"}},
		{"embedded-carriage-return", []string{strings.ReplaceAll(first, ",", ",\r") + "\n"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			messages, err := readStdioFramingFixture(t, test.chunks)
			if err == nil || errors.Is(err, io.EOF) || len(messages) != 0 {
				t.Fatalf("malformed single line must fail before delivery; received %d messages, err=%v", len(messages), err)
			}
		})
	}
}

func TestStdioFramingPreservesValidLineBeforeMalformedLine(t *testing.T) {
	const valid = "{\"jsonrpc\":\"2.0\",\"id\":\"valid\",\"result\":true}"
	const badFirst = "{\"jsonrpc\":\"2.0\",\"id\":\"bad-first\",\"result\":true}"
	const badSecond = "{\"jsonrpc\":\"2.0\",\"id\":\"bad-second\",\"result\":false}"
	for _, test := range []struct {
		name   string
		chunks []string
	}{
		{"coalesced", []string{valid + "\n" + badFirst + badSecond + "\n"}},
		{"fragmented", []string{valid, "\n", badFirst, badSecond + "\n"}},
		{"multiline", []string{valid + "\n{\n" + badFirst[1:] + "\n"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			messages, err := readStdioFramingFixture(t, test.chunks)
			if len(messages) != 1 || string(messages[0].ID()) != "\"valid\"" {
				t.Fatalf("must preserve only the preceding valid line; received %#v, err=%v", messages, err)
			}
			if err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("malformed trailing line must return an error, got %v", err)
			}
		})
	}
}

func TestStdioFramingAcceptsFragmentedMessages(t *testing.T) {
	const first = "{\"jsonrpc\":\"2.0\",\"id\":\"first\",\"result\":\"caf\u00e9\\nline\"}"
	const second = "{\"jsonrpc\":\"2.0\",\"id\":\"second\",\"result\":false}"
	stream := first + "\n" + second + "\n"
	var oneByteChunks []string
	for index := range len(stream) {
		oneByteChunks = append(oneByteChunks, stream[index:index+1])
	}
	for _, test := range []struct {
		name   string
		chunks []string
	}{
		{"two-lines-one-read", []string{stream}},
		{"one-line-per-read", []string{first + "\n", second + "\n"}},
		{"json-framing-whitespace", []string{" \t" + first + " \t\n" + second + "\n"}},
		{"delimiter-in-next-read", []string{first, "\n", second, "\n"}},
		{"inside-json", []string{first[:12], first[12:] + "\n" + second[:17], second[17:] + "\n"}},
		{"split-crlf", []string{first, "\r", "\n", second + "\r", "\n"}},
		{"one-byte-at-a-time-including-utf8", oneByteChunks},
	} {
		t.Run(test.name, func(t *testing.T) {
			messages, err := readStdioFramingFixture(t, test.chunks)
			if !errors.Is(err, io.EOF) || len(messages) != 2 {
				t.Fatalf("valid fragmented stream: received %d messages, err=%v", len(messages), err)
			}
			if string(messages[0].ID()) != "\"first\"" || string(messages[1].ID()) != "\"second\"" {
				t.Fatalf("fragmentation changed message IDs: %s, %s", messages[0].ID(), messages[1].ID())
			}
			if !strings.Contains(string(messages[0]["result"]), "caf\u00e9") {
				t.Fatalf("fragmentation changed UTF-8 result: %s", messages[0]["result"])
			}
		})
	}
}

func TestStdioFramingRequiresFinalDelimiter(t *testing.T) {
	const message = "{\"jsonrpc\":\"2.0\",\"id\":\"valid\",\"result\":true}"
	for _, test := range []struct {
		name   string
		chunks []string
		want   int
	}{
		{"complete-object-without-lf", []string{message}, 0},
		{"partial-object", []string{message[:18]}, 0},
		{"valid-before-unterminated", []string{message + "\n", message}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			messages, err := readStdioFramingFixture(t, test.chunks)
			if len(messages) != test.want || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("unterminated line: received %d messages, err=%v", len(messages), err)
			}
		})
	}
	t.Run("empty-stream", func(t *testing.T) {
		messages, err := readStdioFramingFixture(t, nil)
		if len(messages) != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("empty stream: received %d messages, err=%v", len(messages), err)
		}
	})
}

func TestStdioFramingRejectsInvalidUTF8(t *testing.T) {
	line := "{\"jsonrpc\":\"2.0\",\"id\":\"invalid\",\"result\":\"" + string([]byte{0xff}) + "\"}\n"
	messages, err := readStdioFramingFixture(t, []string{line})
	if len(messages) != 0 || err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("invalid UTF-8: received %d messages, err=%v", len(messages), err)
	}
}

func TestStdioFramingPayloadLimit(t *testing.T) {
	const prefix = "{\"jsonrpc\":\"2.0\",\"id\":\"limit\",\"result\":\""
	const suffix = "\"}"
	payload := prefix + strings.Repeat("x", wire.MaxFrameSize-len(prefix)-len(suffix)) + suffix
	for _, test := range []struct {
		name      string
		delimiter string
		messages  int
	}{
		{"maximum-lf", "\n", 1},
		{"maximum-crlf", "\r\n", 1},
		{"consecutive-maximum-lines", "\n", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var chunks []string
			for range test.messages {
				chunks = append(chunks, payload, test.delimiter)
			}
			messages, err := readStdioFramingFixture(t, chunks)
			if len(messages) != test.messages || !errors.Is(err, io.EOF) {
				t.Fatalf("maximum-sized payload: received %d messages, err=%v", len(messages), err)
			}
		})
	}
	t.Run("one-byte-over", func(t *testing.T) {
		oversized := payload[:len(payload)-len(suffix)] + "x" + suffix
		messages, err := readStdioFramingFixture(t, []string{oversized, "\n"})
		if len(messages) != 0 || err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("oversized payload: received %d messages, err=%v", len(messages), err)
		}
	})
}
