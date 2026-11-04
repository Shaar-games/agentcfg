package bootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentcfg/internal/config"
	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
)

var errInactiveServer = errors.New("a disabled native server was skipped")
var errNativeRuntime = errors.New("Codex desktop session runtime is kept in its native configuration")

func discoverServers(root *os.Root, plan *Plan, target target, source string) {
	path, err := anchoredPath(root, source)
	if err != nil {
		plan.warn(target.name, source, "native config changed or escapes the discovery home")
		return
	}
	data, err := readFile(root, path, maxConfigBytes, true)
	if err != nil {
		plan.warn(target.name, source, "native config is unreadable, nonregular, invalid UTF-8 or larger than 1 MiB")
		return
	}
	if err := captureSourceHash(root, plan, source, path, sha256.Sum256(data)); err != nil {
		plan.warn(target.name, source, "native source changed during discovery; refresh the import preview")
		return
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if target.toml {
		var document map[string]any
		if err = toml.Unmarshal(data, &document); err == nil {
			data, err = json.Marshal(document)
		}
	} else {
		data, err = hujson.Standardize(data)
	}
	if err != nil {
		plan.warn(target.name, source, "native config has invalid syntax")
		return
	}
	document, err := object(data)
	if err != nil {
		plan.warn(target.name, source, "native config must be an object without duplicate keys")
		return
	}
	raw, present := document[target.key]
	if !present {
		return
	}
	entries, err := object(raw)
	if err != nil {
		plan.warn(target.name, source, "native MCP section must be an object without duplicate names")
		return
	}
	// These global policies cannot be represented by a canonical server entry.
	// Do not broaden their effect when importing to another harness.
	if target.name == "opencode" && document["tools"] != nil ||
		target.name == "gemini" && document["mcp"] != nil {
		plan.warn(target.name, source, "native MCP/tool policy requires manual import")
		return
	}
	disabled := map[string]bool{}
	if raw := document["disabledMcpServers"]; raw != nil {
		names, err := stringList(raw)
		if err != nil {
			plan.warn(target.name, source, "native disabled-server policy is invalid; MCP import skipped")
			return
		}
		for _, name := range names {
			disabled[name] = true
		}
	}
	for _, name := range keys(entries) {
		if plan.budget.servers == 0 {
			plan.warn(target.name, source, "discovery server limit reached; remaining entries require a separate review")
			break
		}
		plan.budget.servers--
		if disabled[name] {
			plan.Warnings = append(plan.Warnings, Warning{Harness: target.name, Source: source, Server: name, Message: errInactiveServer.Error(), Kind: WarningInactive})
			continue
		}
		imported, err := convertServer(root, target.name, name, entries[name])
		if err != nil {
			// Errors are intentionally constant descriptions, never parser errors
			// or native values that could contain credentials.
			var kind WarningKind
			if errors.Is(err, errInactiveServer) {
				kind = WarningInactive
			}
			if errors.Is(err, errNativeRuntime) {
				kind = WarningNativeRuntime
			}
			plan.Warnings = append(plan.Warnings, Warning{Harness: target.name, Source: source, Server: name, Message: err.Error(), Kind: kind})
			continue
		}
		server := imported.server
		server.Source = source
		for _, message := range append(imported.advisories, importAdvisories(server)...) {
			plan.Warnings = append(plan.Warnings, Warning{Harness: target.name, Source: source, Server: name, Message: message, Kind: WarningAdvisory})
		}
		plan.Servers = append(plan.Servers, ServerCandidate{
			Name: name, Source: source, Sources: []string{source},
			Harnesses: []string{target.name}, Server: server,
			Origins:  []Origin{{Harness: target.name, Source: source}},
			Selected: true, Warnings: imported.warnings,
		})
	}
}

func convertServer(root *os.Root, harness, name string, raw json.RawMessage) (nativeServerImport, error) {
	fail := func(message string) (nativeServerImport, error) {
		return nativeServerImport{}, errors.New(message)
	}
	if !validName(name) {
		return fail("a native server has an invalid name")
	}
	fields, err := object(raw)
	if err != nil {
		return fail("a native server is not an object or has duplicate fields")
	}
	for _, key := range []string{"enabled", "disabled"} {
		if value, exists := fields[key]; exists {
			var flag bool
			if string(value) == "null" || json.Unmarshal(value, &flag) != nil {
				return fail("a native enable/disable flag is invalid")
			}
			if key == "enabled" && !flag || key == "disabled" && flag {
				return nativeServerImport{}, errInactiveServer
			}
		}
	}
	if fields["enabled"] != nil && fields["disabled"] != nil {
		return fail("a native server has ambiguous enable/disable flags")
	}
	if codexDesktopRuntime(harness, name, fields) {
		return nativeServerImport{}, errNativeRuntime
	}
	allowed := map[string]bool{"type": true, "command": true, "cwd": true, "enabled": true, "disabled": true}
	envKey, headerKey, urlKey := "env", "headers", "url"
	switch harness {
	case "opencode":
		envKey = "environment"
		allowed["environment"], allowed["url"], allowed["headers"] = true, true, true
	case "codex":
		headerKey = "http_headers"
		allowed["args"], allowed["env"], allowed["url"], allowed["http_headers"] = true, true, true, true
	case "gemini":
		urlKey = "httpUrl"
		allowed["args"], allowed["env"], allowed["httpUrl"], allowed["url"], allowed["headers"] = true, true, true, true, true
	case "antigravity":
		allowed["args"], allowed["env"], allowed["serverUrl"], allowed["url"], allowed["headers"] = true, true, true, true, true
		if fields["serverUrl"] != nil {
			urlKey = "serverUrl"
		}
	default:
		allowed["args"], allowed["env"], allowed["url"], allowed["headers"] = true, true, true, true
	}
	var ignored, unsupported []string
	for _, key := range keys(fields) {
		if !allowed[key] {
			if ignorableNativeSetting(harness, key) {
				ignored = append(ignored, fmt.Sprintf("%q", key))
			} else {
				unsupported = append(unsupported, fmt.Sprintf("%q", key))
			}
		}
	}
	if len(unsupported) > 0 {
		return fail("unsupported native option(s): " + strings.Join(unsupported, ", ") + "; manual import required")
	}
	var advisories []string
	if len(ignored) > 0 {
		advisories = append(advisories, "native timing settings "+strings.Join(ignored, ", ")+" are not imported; agentcfg timeouts apply")
	}
	kind, err := stringValue(fields["type"])
	if err != nil {
		return fail("a native server has an invalid transport type")
	}
	server := config.Server{Name: name, Sharing: "shared", Harnesses: []string{harness}}
	var warnings []string
	isCommand, isURL := fields["command"] != nil, fields[urlKey] != nil
	if harness == "gemini" && fields["url"] != nil {
		return fail("a legacy SSE or mixed Gemini endpoint was skipped")
	}
	if harness == "antigravity" && fields["serverUrl"] != nil && fields["url"] != nil {
		return fail("a native server defines multiple endpoint URLs")
	}
	if isCommand == isURL {
		return fail("a native server must define exactly one command or HTTP endpoint")
	}
	if isCommand {
		if kind != "" && kind != "stdio" && !(harness == "opencode" && kind == "local") {
			return fail("a native server transport does not match its command")
		}
		if harness == "opencode" && kind != "local" {
			return fail("an OpenCode local server requires type local")
		}
		if fields[headerKey] != nil {
			return fail("a native stdio server contains HTTP settings")
		}
		server.Type = "stdio"
		if harness == "opencode" {
			argv, parseErr := stringList(fields["command"])
			if parseErr != nil || len(argv) == 0 {
				return fail("an OpenCode command must be a nonempty string array")
			}
			server.Command, server.Args = argv[0], argv[1:]
		} else {
			server.Command, err = stringValue(fields["command"])
			if err != nil {
				return fail("a native command must be a string")
			}
			server.Args, err = stringList(fields["args"])
			if err != nil {
				return fail("native command arguments must be strings")
			}
		}
		if isManagedProxy(server) {
			return fail("an existing agentcfg proxy was skipped")
		}
		server.Env, err = stringMap(fields[envKey])
		if err != nil {
			return fail("native environment must be a string map without duplicate keys")
		}
		server.Cwd, err = stringValue(fields["cwd"])
		if err != nil {
			return fail("native cwd must be a string")
		}
		if server.Cwd == "" && fields["cwd"] != nil {
			return fail("an empty explicit native cwd is ambiguous")
		}
		if err = validateProcess(root, &server); err != nil {
			return fail(err.Error())
		}
		if server.Cwd != "" && !within(root.Name(), server.Cwd) || filepath.IsAbs(server.Command) && !within(root.Name(), server.Command) {
			warnings = append(warnings, "external executable/cwd paths are retained as references; existence is checked by canonical loading, not discovery")
		}
	} else {
		if kind != "" && kind != "http" && kind != "streamable-http" && !(harness == "opencode" && kind == "remote") {
			return fail("legacy SSE or unsupported transport was skipped")
		}
		if harness == "opencode" && kind != "remote" {
			return fail("an OpenCode remote server requires type remote")
		}
		if fields["args"] != nil || fields[envKey] != nil || fields["cwd"] != nil {
			return fail("a native HTTP server contains process settings")
		}
		server.Type = "streamable-http"
		server.URL, err = stringValue(fields[urlKey])
		if err != nil {
			return fail("a native endpoint must be a string")
		}
		server.Headers, err = stringMap(fields[headerKey])
		if err != nil {
			return fail("native headers must be a string map without duplicate keys")
		}
		if err = validateRemote(server); err != nil {
			return fail(err.Error())
		}
		warnings = append(warnings, "stored native OAuth credentials are not imported; authenticate separately if required")
	}
	return nativeServerImport{server: server, warnings: warnings, advisories: advisories}, nil
}

func object(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected object")
	}
	result := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid object")
		}
		key, ok := token.(string)
		if !ok || result[key] != nil {
			return nil, errors.New("duplicate key")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("invalid value")
		}
		result[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, errors.New("invalid object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return result, nil
}

func stringValue(raw json.RawMessage) (string, error) {
	if raw == nil {
		return "", nil
	}
	var value string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return "", errors.New("expected string")
	}
	return value, nil
}

func stringList(raw json.RawMessage) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	var values []json.RawMessage
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &values) != nil {
		return nil, errors.New("expected array")
	}
	result := make([]string, 0, len(values))
	for _, raw := range values {
		value, err := stringValue(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

func stringMap(raw json.RawMessage) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	values, err := object(raw)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for key, raw := range values {
		value, err := stringValue(raw)
		if err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, nil
}

func keys[T any](values map[string]T) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func isManagedProxy(server config.Server) bool {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(server.Command)), ".exe")
	return base == "agentcfg" && len(server.Args) > 0 && server.Args[0] == "connect"
}
