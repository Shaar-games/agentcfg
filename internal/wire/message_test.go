package wire

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestFramingRoundTripPreservesUnknownFields(t *testing.T) {
	message, err := Decode([]byte(`{"jsonrpc":"2.0","id":"same","method":"tools/call","vendor":{"x":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	copy := message.Clone()
	copy["id"] = Raw(42)
	if string(message.ID()) != `"same"` {
		t.Fatal("clone mutated original")
	}
	var buffer bytes.Buffer
	if err := NewWriter(&buffer).Write(copy); err != nil {
		t.Fatal(err)
	}
	actual, err := NewReader(&buffer).Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(actual["vendor"]) != `{"x":true}` || string(actual.ID()) != "42" {
		t.Fatal(actual)
	}
}

func TestRejectMalformedAndOversizedFrames(t *testing.T) {
	for _, input := range []string{"null", "[]", "{broken", `"string"`} {
		if _, err := Decode([]byte(input)); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	reader := NewReader(io.MultiReader(strings.NewReader(`{"x":"`), io.LimitReader(zeroReader{}, MaxFrameSize), strings.NewReader("\"}\n")))
	if _, err := reader.Read(); err == nil {
		t.Fatal("accepted oversized frame")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestConcurrentWritesRemainWholeFrames(t *testing.T) {
	var buffer bytes.Buffer
	writer := NewWriter(&buffer)
	var group sync.WaitGroup
	for i := range 50 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := writer.Write(Result(Raw(i), map[string]int{"n": i})); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	lines := bytes.Split(bytes.TrimSpace(buffer.Bytes()), []byte("\n"))
	if len(lines) != 50 {
		t.Fatal(len(lines))
	}
	for _, line := range lines {
		if !json.Valid(line) {
			t.Fatal(string(line))
		}
	}
}
