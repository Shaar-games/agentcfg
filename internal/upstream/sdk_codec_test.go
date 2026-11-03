package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"agentcfg/internal/wire"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSDKCodecPreservesRawExtensions(t *testing.T) {
	messages := []wire.Message{
		{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw("request"), "method": wire.Raw("future/method"),
			"params": json.RawMessage(`{"_meta":{"future":{"opaque":true}},"large":900719925474099312345,"requestState":"keep","inputResponses":{"sample":{"content":[{"type":"text","text":"hello"}]}},"extension":{"untyped":[1,2,3]}}`)},
		{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw("request"), "result": json.RawMessage(`{"resultType":"input_required","future":900719925474099312345,"extension":true}`)},
		{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw("request"), "error": json.RawMessage(`{"code":-32022,"message":"version","data":{"supportedVersions":["2026-07-28"]}}`)},
	}
	for _, message := range messages {
		decoded, err := decodeSDKMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := encodeSDKMessage(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(message, encoded) {
			t.Fatalf("SDK codec altered raw fields:\nwant %#v\ngot %#v", message, encoded)
		}
	}
}

func TestSDKCodecRejectsLossyEnvelopeAndIdentifier(t *testing.T) {
	message := rpcRequest("id", "future/method", nil)
	message["extension"] = wire.Raw(true)
	if _, err := decodeSDKMessage(message); !errors.Is(err, errSDKEnvelope) {
		t.Fatalf("SDK would silently lose an envelope extension: %v", err)
	}
	delete(message, "extension")
	for _, id := range []string{"1.5", "9007199254740993", "null"} {
		message["id"] = json.RawMessage(id)
		if _, err := decodeSDKMessage(message); !errors.Is(err, errSDKIdentifier) {
			t.Fatalf("SDK would silently coerce ID %s: %v", id, err)
		}
	}
}

type discardWriteCloser struct{ io.Writer }

func (discardWriteCloser) Close() error { return nil }

func TestSDKIORejectsBatchesAndPreservesPriorFrame(t *testing.T) {
	reader := &singleMessageReader{ReadCloser: io.NopCloser(strings.NewReader(
		"{\"jsonrpc\":\"2.0\",\"id\":\"ok\",\"result\":true}\n [{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":false}]\n")), atStart: true}
	connection, err := (&mcp.IOTransport{Reader: reader, Writer: discardWriteCloser{io.Discard}, MaxLineLength: wire.MaxFrameSize}).Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	message, err := connection.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := encodeSDKMessage(message)
	if err != nil || string(decoded.ID()) != `"ok"` {
		t.Fatalf("prior frame lost: %#v %v", decoded, err)
	}
	if _, err = connection.Read(context.Background()); err == nil {
		t.Fatal("SDK accepted unsupported batch")
	}
}

func TestSDKStdioRepeatsEOFWithoutBlocking(t *testing.T) {
	transport := startTestTransport(t, context.Background(), helperServer(t, "exit"))
	mustRead(t, transport)
	for range 2 {
		if _, err := readResult(t, transport); !errors.Is(err, io.EOF) {
			t.Fatalf("EOF changed: %v", err)
		}
	}
}
