package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

func readBounded(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("expected a regular file")
	}
	if info.Size() > maxConfigBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", maxConfigBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", maxConfigBytes)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("file must be UTF-8")
	}
	return data, nil
}

func object(data []byte) (map[string]json.RawMessage, error) {
	// Windows PowerShell 5.1 emits UTF-8 BOMs with Set-Content -Encoding UTF8.
	// JSON readers may ignore an initial BOM; do not rewrite the source file.
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, fmt.Errorf("expected a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("JSON object key must be a string")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate JSON key %q", key)
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err = decoder.Token(); err != nil {
		return nil, err
	}
	if _, err = decoder.Token(); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unexpected content after JSON document")
	}
	// Nested values remain raw until their owning component is validated.
	// A wrong-typed or deeply nested server cannot disable valid siblings.
	return fields, nil
}

func allowFields(fields map[string]json.RawMessage, allowed ...string) error {
	known := map[string]bool{}
	for _, name := range allowed {
		known[name] = true
	}
	for _, name := range sortedKeys(fields) {
		if !known[name] {
			return fmt.Errorf("unknown field %q", name)
		}
	}
	return nil
}

func stringField(fields map[string]json.RawMessage, name string, required bool) (string, error) {
	raw, exists := fields[name]
	if !exists {
		if required {
			return "", fmt.Errorf("missing required field %q", name)
		}
		return "", nil
	}
	var value string
	if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return value, nil
}

func stringList(raw json.RawMessage, name string) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	var values []json.RawMessage
	if string(raw) == "null" || json.Unmarshal(raw, &values) != nil {
		return nil, fmt.Errorf("%s must be an array of strings", name)
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		var text string
		if string(value) == "null" || json.Unmarshal(value, &text) != nil {
			return nil, fmt.Errorf("%s must contain only strings", name)
		}
		result = append(result, text)
	}
	return result, nil
}

func stringMap(raw json.RawMessage, name string) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	fields, err := object(raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be an object: %w", name, err)
	}
	result := make(map[string]string, len(fields))
	for _, key := range sortedKeys(fields) {
		value, err := stringField(fields, key, true)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		result[key] = value
	}
	return result, nil
}

func rejectNUL(value, field string) error {
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s contains a NUL byte", field)
	}
	return nil
}
