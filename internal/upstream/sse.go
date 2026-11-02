package upstream

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"unicode/utf8"

	"agentmux/internal/wire"
)

var errStreamComplete = errors.New("MCP response complete")

// sseSplitter accepts CR immediately. Waiting for the optional LF would stall
// a flushed bare-CR event on a server that is waiting for our callback response.
type sseSplitter struct{ skipLF bool }

func (s *sseSplitter) split(data []byte, atEOF bool) (int, []byte, error) {
	offset := 0
	if s.skipLF && len(data) > 0 {
		s.skipLF = false
		if data[0] == '\n' {
			offset = 1
		}
	}
	for i := offset; i < len(data); i++ {
		c := data[i]
		if c == '\n' {
			return i + 1, data[offset:i], nil
		}
		if c == '\r' {
			s.skipLF = true
			return i + 1, data[offset:i], nil
		}
	}
	if atEOF && len(data) > offset {
		return len(data), data[offset:], nil
	}
	return offset, nil, nil
}

func readSSE(reader io.Reader, receive func(wire.Message) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 32<<10), wire.MaxFrameSize+1)
	scanner.Split((&sseSplitter{}).split)
	var data []byte
	event := ""
	eventBytes := 0
	first := true
	for scanner.Scan() {
		line := scanner.Bytes()
		if first {
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
			first = false
		}
		if len(line) == 0 {
			if len(data) > 1 && (event == "" || event == "message") {
				payload := data[:len(data)-1]
				if !utf8.Valid(payload) {
					return errors.New("MCP SSE message is not valid UTF-8")
				}
				message, err := wire.Decode(payload)
				if err != nil {
					return errors.New("invalid or oversized MCP SSE JSON-RPC message")
				}
				if err := receive(message); err != nil {
					return err
				}
			}
			data = data[:0]
			event = ""
			eventBytes = 0
			continue
		}
		eventBytes += len(line) + 1
		if eventBytes > wire.MaxFrameSize {
			return errors.New("MCP SSE event exceeds 32 MiB")
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			event = string(value)
		case "data":
			if len(data)+len(value)+1 > wire.MaxFrameSize {
				return errors.New("MCP SSE message exceeds 32 MiB")
			}
			data = append(data, value...)
			data = append(data, '\n')
		}
	}
	if scanner.Err() != nil {
		return errors.New("MCP SSE stream read failed or frame exceeds 32 MiB")
	}
	// SSE only dispatches a complete event after a blank line; an unterminated
	// final event must not be interpreted as a successful JSON-RPC response.
	return nil
}
