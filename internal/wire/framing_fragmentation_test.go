package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// readerFixedFragments makes the underlying read sizes independent of pipe or
// socket buffering, including when the scanner grows or reuses its buffer.
type readerFixedFragments struct {
	source io.Reader
	size   int
}

func (r readerFixedFragments) Read(data []byte) (int, error) {
	if len(data) > r.size {
		data = data[:r.size]
	}
	return r.source.Read(data)
}

func TestReaderFragmentedLargeLinesPreserveFollowingMessages(t *testing.T) {
	large := strings.Repeat("x", 8<<20)
	want := []Message{
		Result(Raw("large-first"), large),
		Result(Raw("small"), "caf\u00e9"),
		Result(Raw("large-second"), large+"tail"),
		Result(Raw("large-third"), large),
	}
	for _, delimiter := range []struct {
		name string
		text string
	}{
		{"LF", "\n"},
		{"CRLF", "\r\n"},
	} {
		var stream bytes.Buffer
		for _, message := range want {
			data, err := json.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			stream.Write(data)
			stream.WriteString(delimiter.text)
		}
		for _, fragment := range []struct {
			name string
			size int
		}{
			{"1KiB", 1 << 10},
			{"64KiB", 64 << 10},
		} {
			t.Run(delimiter.name+"/"+fragment.name, func(t *testing.T) {
				reader := NewReader(readerFixedFragments{bytes.NewReader(stream.Bytes()), fragment.size})
				for index, expected := range want {
					actual, err := reader.Read()
					if err != nil {
						t.Fatalf("message %d: %v", index, err)
					}
					if !reflect.DeepEqual(actual, expected) {
						t.Fatalf("message %d changed after a fragmented line; got ID %s, want %s", index, actual.ID(), expected.ID())
					}
				}
				for range 2 {
					if _, err := reader.Read(); !errors.Is(err, io.EOF) {
						t.Fatalf("expected stable EOF after the final message, got %v", err)
					}
				}
			})
		}
	}
}

func TestReaderFragmentedLineEndingsKeepScanLinesSemantics(t *testing.T) {
	const message = `{"jsonrpc":"2.0","id":"ending","result":"café"}`
	for _, test := range []struct {
		name  string
		input string
		count int
	}{
		{"LF", message + "\n", 1},
		{"CRLF", message + "\r\n", 1},
		{"EOF-without-LF", message, 1},
		{"CR-at-EOF", message + "\r", 1},
		{"terminated-then-EOF-line", message + "\n" + message, 2},
		{"blank-lines", "\n\r\n\t \r\n" + message + "\n", 1},
		{"empty-stream", "", 0},
		{"blank-EOF", " \t", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := NewReader(readerFixedFragments{strings.NewReader(test.input), 1})
			for range test.count {
				actual, err := reader.Read()
				if err != nil || string(actual.ID()) != `"ending"` {
					t.Fatalf("line ending changed the message: ID=%s err=%v", actual.ID(), err)
				}
				var result string
				if err := json.Unmarshal(actual["result"], &result); err != nil || result != "caf\u00e9" {
					t.Fatalf("fragmentation changed UTF-8 result: %q, %v", result, err)
				}
			}
			for range 2 {
				if _, err := reader.Read(); !errors.Is(err, io.EOF) {
					t.Fatalf("expected stable EOF after %d messages, got %v", test.count, err)
				}
			}
		})
	}
}

func benchmarkReaderFrame(size int) []byte {
	const prefix = `{"jsonrpc":"2.0","id":"bench","result":"`
	const suffix = `"}`
	return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
}

func BenchmarkReaderFixedFragments(b *testing.B) {
	for _, test := range []struct {
		name      string
		size      int
		fragment  int
		oversized bool
	}{
		{"8MiB/1KiB", 8 << 20, 1 << 10, false},
		{"8MiB/64KiB", 8 << 20, 64 << 10, false},
		{"32MiB/1KiB", MaxFrameSize, 1 << 10, false},
		{"32MiB/64KiB", MaxFrameSize, 64 << 10, false},
		{"oversized/1KiB", MaxFrameSize + 2, 1 << 10, true},
		{"oversized/64KiB", MaxFrameSize + 2, 64 << 10, true},
	} {
		b.Run(test.name, func(b *testing.B) {
			data := benchmarkReaderFrame(test.size)
			if !test.oversized {
				data = append(data, '\n')
			}
			b.SetBytes(int64(test.size))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				reader := NewReader(readerFixedFragments{bytes.NewReader(data), test.fragment})
				message, err := reader.Read()
				if test.oversized {
					if err == nil {
						b.Fatal("oversized line was accepted")
					}
				} else if err != nil || string(message.ID()) != `"bench"` {
					b.Fatalf("valid fragmented message changed: ID=%s err=%v", message.ID(), err)
				}
			}
		})
	}
}
