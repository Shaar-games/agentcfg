package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"agentmux/internal/wire"
)

func TestSSEFraming(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(newline, "\r", "CR"), "\n", "LF"), func(t *testing.T) {
			input := strings.Join([]string{"\xef\xbb\xbf: comment", "id: cursor", "data:", "", "event: message", "data: {\"jsonrpc\":\"2.0\",", "data: \"method\":\"notifications/test\"}", "", ""}, newline)
			count := 0
			err := readSSE(strings.NewReader(input), func(message wire.Message) error {
				count++
				if message.Method() != "notifications/test" {
					t.Error("wrong SSE payload")
				}
				return nil
			})
			if err != nil || count != 1 {
				t.Fatalf("readSSE: count=%d, err=%v", count, err)
			}
		})
	}
}

func TestSSEDispatchesBareCRBeforeEOF(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	done := make(chan error, 1)
	go func() { done <- readSSE(reader, func(wire.Message) error { return errStreamComplete }) }()
	if _, err := io.WriteString(writer, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notice\"}\r\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errStreamComplete) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("bare-CR event waited for another byte or EOF")
	}
}

func TestSSECRLFAcrossReadBoundaries(t *testing.T) {
	input := "data: {\"jsonrpc\":\"2.0\",\"method\":\"notice\"}\r\n\r\n"
	count := 0
	err := readSSE(iotest.OneByteReader(strings.NewReader(input)), func(wire.Message) error { count++; return nil })
	if err != nil || count != 1 {
		t.Fatalf("split CRLF framing: count=%d err=%v", count, err)
	}
}

func TestSSERejectsMalformedOversizedAndIncompleteEvents(t *testing.T) {
	for _, input := range []string{
		"data: not-json\n\n",
		"data: " + strings.Repeat("x", wire.MaxFrameSize+1) + "\n\n",
		strings.Repeat("data: "+strings.Repeat("x", 1024)+"\n", wire.MaxFrameSize/1024+1) + "\n",
	} {
		if err := readSSE(strings.NewReader(input), func(wire.Message) error { return nil }); err == nil {
			t.Fatal("bad SSE event accepted")
		}
	}
	called := false
	err := readSSE(strings.NewReader("data: {\"method\":\"notice\"}\n"), func(wire.Message) error { called = true; return nil })
	if err != nil || called {
		t.Fatal("incomplete trailing SSE event was dispatched")
	}
}

func TestRequestIDKeysPreserveTypesAndPrecision(t *testing.T) {
	for _, pair := range [][2]string{{`"abc"`, `"\u0061bc"`}, {`1`, `1.00`}, {`10`, `1e1`}, {`-0`, `0e50`}} {
		if requestIDKey(json.RawMessage(pair[0])) != requestIDKey(json.RawMessage(pair[1])) {
			t.Errorf("IDs differ: %v", pair)
		}
	}
	for _, pair := range [][2]string{{`1`, `"1"`}, {`9007199254740992`, `9007199254740993`}, {`-1`, `1`}} {
		if requestIDKey(json.RawMessage(pair[0])) == requestIDKey(json.RawMessage(pair[1])) {
			t.Errorf("IDs collapsed: %v", pair)
		}
	}
}

func TestHTTPReceiveBackpressureHonorsCancellation(t *testing.T) {
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	transport := &httpTransport{ctx: lifetime, cancel: stop, incoming: make(chan routedMessage, 1), pending: make(map[string]*httpPending)}
	transport.incoming <- routedMessage{message: wire.Notification("full", nil)}
	ctx, cancel := context.WithCancel(lifetime)
	done := make(chan error, 1)
	go func() { done <- transport.emit(ctx, wire.Notification("blocked", nil)) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked delivery ignored cancellation")
	}
	transport.postFailure(wire.Raw("id"), true, false, false, context.Canceled, false)
	if lifetime.Err() == nil {
		t.Fatal("undeliverable completion should stop the transport")
	}
}
