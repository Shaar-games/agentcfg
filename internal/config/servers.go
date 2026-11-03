package config

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

func serverDocument(data []byte, portable bool) (map[string]json.RawMessage, error) {
	fields, err := object(data)
	if err != nil {
		return nil, err
	}
	if err = allowFields(fields, "$schema", "mcpServers"); err != nil {
		return nil, err
	}
	if portable || fields["$schema"] != nil {
		schema, err := stringField(fields, "$schema", true)
		if err != nil {
			return nil, err
		}
		if schema != mcpSchema {
			return nil, fmt.Errorf("unsupported MCP $schema %q; expected %s", schema, mcpSchema)
		}
	}
	servers, exists := fields["mcpServers"]
	if !exists {
		return nil, fmt.Errorf("missing required mcpServers object")
	}
	entries, err := object(servers)
	if err != nil {
		return nil, fmt.Errorf("mcpServers: %w", err)
	}
	return entries, nil
}

func validEntryName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	return strings.IndexFunc(name, unicode.IsControl) == -1
}

func (l *loader) loadServerEntries(entries map[string]json.RawMessage, source, pluginRoot, pluginName string) {
	for _, name := range sortedKeys(entries) {
		id := name
		if pluginName != "" {
			id = pluginName + "." + name
		}
		location := source + ": server " + strconv.Quote(id)
		if !validEntryName(name) {
			l.warn(location, fmt.Errorf("server name must be nonempty and contain no control characters"))
			continue
		}
		server, err := parseServer(entries[name], l.config.Root, pluginRoot, pluginName)
		if err != nil {
			l.warn(location, err)
			continue
		}
		server.Name, server.Source = id, source
		l.addServer(server)
	}
}

func parseServer(raw json.RawMessage, root, pluginRoot, pluginName string) (Server, error) {
	fields, err := object(raw)
	if err != nil {
		return Server{}, err
	}
	portable := pluginRoot != ""
	kind, err := stringField(fields, "type", portable)
	if err != nil {
		return Server{}, err
	}
	if kind == "" && !portable && fields["type"] == nil {
		if fields["command"] != nil {
			kind = "stdio"
		} else if fields["url"] != nil {
			kind = "streamable-http"
		}
	}
	allowed := []string{"type"}
	switch kind {
	case "stdio":
		allowed = append(allowed, "command", "args", "env", "cwd")
	case "streamable-http", "sse":
		allowed = append(allowed, "url", "headers")
	default:
		return Server{}, fmt.Errorf("unsupported transport %q; use stdio or streamable-http", kind)
	}
	if !portable {
		allowed = append(allowed, "harnesses", "sharing", "oauth")
	}
	if err = allowFields(fields, allowed...); err != nil {
		if portable {
			return Server{}, fmt.Errorf("%w; agentcfg harnesses/sharing/oauth belong in agentcfg.json serverOverrides", err)
		}
		return Server{}, err
	}
	server := Server{Type: kind, Sharing: "shared"}
	if !portable {
		if err = parseServerOptions(fields, &server); err != nil {
			return Server{}, err
		}
	}
	if kind == "stdio" {
		err = parseStdio(fields, &server, root, pluginRoot, pluginName)
	} else {
		err = parseRemote(fields, &server)
	}
	if err != nil {
		return Server{}, err
	}
	if kind == "sse" {
		return Server{}, fmt.Errorf("legacy sse transport is unsupported; use a streamable-http endpoint (SSE responses within Streamable HTTP are supported separately)")
	}
	if err = validateServerOAuth(server); err != nil {
		return Server{}, err
	}
	return server, nil
}

func parseServerOptions(fields map[string]json.RawMessage, server *Server) error {
	var err error
	if raw, exists := fields["oauth"]; exists {
		server.OAuth, err = parseOAuth(raw)
		if err != nil {
			return err
		}
	}
	if raw, exists := fields["harnesses"]; exists {
		server.Harnesses, err = parseHarnesses(raw)
		if err != nil {
			return err
		}
	}
	if _, exists := fields["sharing"]; exists {
		value, err := stringField(fields, "sharing", true)
		if err != nil {
			return err
		}
		if value == "" {
			return fmt.Errorf("sharing must be shared or isolated")
		}
		server.Sharing, err = parseSharing(value)
		if err != nil {
			return err
		}
	}
	return nil
}

func parseStdio(fields map[string]json.RawMessage, server *Server, root, pluginRoot, pluginName string) error {
	var err error
	server.Command, err = stringField(fields, "command", true)
	if err != nil {
		return err
	}
	server.Args, err = stringList(fields["args"], "args")
	if err != nil {
		return err
	}
	server.Env, err = stringMap(fields["env"], "env")
	if err != nil {
		return err
	}
	server.Cwd, err = stringField(fields, "cwd", false)
	if err != nil {
		return err
	}
	if err = validateProcessStrings(*server, pluginRoot != ""); err != nil {
		return err
	}
	if pluginRoot == "" {
		return resolveCanonicalProcess(server, root)
	}
	server.Command, err = resolvePluginCommand(server.Command, pluginRoot)
	if err != nil {
		return err
	}
	data, err := pluginDataRoot(root, pluginName)
	if err != nil {
		return err
	}
	if _, present := fields["cwd"]; present && server.Cwd == "" {
		return fmt.Errorf("plugin cwd must not be empty when specified")
	}
	server.Cwd, err = pluginWorkingDirectory(server.Cwd, pluginRoot, data)
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	if info, statErr := os.Stat(server.Cwd); statErr == nil && !info.IsDir() {
		return fmt.Errorf("cwd must be a directory")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	for i, value := range server.Args {
		server.Args[i] = expandPlugin(value, pluginRoot, data)
	}
	if server.Env == nil {
		server.Env = map[string]string{}
	}
	for key, value := range server.Env {
		server.Env[key] = expandPlugin(value, pluginRoot, data)
	}
	server.Env["PLUGIN_ROOT"], server.Env["PLUGIN_DATA"] = pluginRoot, data
	return nil
}

func validateProcessStrings(server Server, portable bool) error {
	if strings.TrimSpace(server.Command) == "" {
		return fmt.Errorf("command must contain one executable token")
	}
	if err := rejectNUL(server.Command, "command"); err != nil {
		return err
	}
	if err := rejectNUL(server.Cwd, "cwd"); err != nil {
		return err
	}
	for _, value := range server.Args {
		if err := rejectNUL(value, "args"); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, key := range sortedKeys(server.Env) {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return fmt.Errorf("invalid environment variable name %q", key)
		}
		if portable && (strings.EqualFold(key, "PLUGIN_ROOT") || strings.EqualFold(key, "PLUGIN_DATA")) {
			return fmt.Errorf("environment variable %s is reserved", key)
		}
		folded := strings.ToUpper(key)
		if seen[folded] {
			return fmt.Errorf("environment names differ only by case: %s", key)
		}
		seen[folded] = true
		if err := rejectNUL(server.Env[key], "env."+key); err != nil {
			return err
		}
	}
	return nil
}

func bareCommand(command string) bool {
	return command != "." && command != ".." && !strings.ContainsAny(command, "/\\:") &&
		strings.IndexFunc(command, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) == -1
}

func resolvePluginCommand(command, root string) (string, error) {
	if bareCommand(command) {
		return command, nil
	}
	resolved, err := pluginRelative(root, command, false)
	if err != nil {
		return "", fmt.Errorf("command: %w (put arguments in args)", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("command must resolve to a regular file")
	}
	return resolved, nil
}

func resolveCanonicalProcess(server *Server, root string) error {
	if !bareCommand(server.Command) {
		path := server.Command
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		resolved, err := resolveExisting(path)
		if err != nil {
			return fmt.Errorf("command: %w (put arguments in args)", err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("command must resolve to a regular file")
		}
		server.Command = resolved
	}
	if server.Cwd == "" {
		server.Cwd = root
	}
	if !filepath.IsAbs(server.Cwd) {
		server.Cwd = filepath.Join(root, server.Cwd)
	}
	resolved, err := resolveExisting(server.Cwd)
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	if err = requireDirectory(resolved); err != nil {
		return err
	}
	server.Cwd = resolved
	return nil
}

func parseRemote(fields map[string]json.RawMessage, server *Server) error {
	var err error
	server.URL, err = stringField(fields, "url", true)
	if err != nil {
		return err
	}
	if err = validateURL(server.URL); err != nil {
		return err
	}
	server.Headers, err = stringMap(fields["headers"], "headers")
	if err != nil {
		return err
	}
	return validateHeaders(server.Headers)
}

func validateURL(value string) error {
	endpoint, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("invalid MCP URL: %w", err)
	}
	if (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Hostname() == "" || endpoint.Opaque != "" {
		return fmt.Errorf("MCP URL must be an absolute http or https URL")
	}
	if endpoint.User != nil || strings.Contains(value, "#") {
		return fmt.Errorf("MCP URL must not contain user information or a fragment")
	}
	if strings.ContainsAny(endpoint.Host, "\\ \t\r\n") {
		return fmt.Errorf("invalid MCP URL host")
	}
	if strings.Contains(endpoint.Hostname(), ":") && !strings.HasPrefix(endpoint.Host, "[") {
		return fmt.Errorf("IPv6 MCP URL hosts must use brackets")
	}
	ip, ipErr := netip.ParseAddr(endpoint.Hostname())
	if strings.HasPrefix(endpoint.Host, "[") && (ipErr != nil || !ip.Is6()) {
		return fmt.Errorf("bracketed MCP URL host must be an IP literal")
	}
	if port := endpoint.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("MCP URL port must be between 1 and 65535")
		}
	} else if strings.HasSuffix(endpoint.Host, ":") {
		return fmt.Errorf("MCP URL port must not be empty")
	}
	if endpoint.Scheme == "http" {
		host := endpoint.Hostname()
		if host != "localhost" && (ipErr != nil || !ip.IsLoopback()) {
			return fmt.Errorf("non-loopback MCP endpoints must use https")
		}
	}
	return nil
}

func validateHeaders(headers map[string]string) error {
	seen := map[string]bool{}
	for _, name := range sortedKeys(headers) {
		if name == "" {
			return fmt.Errorf("HTTP header name must not be empty")
		}
		for _, char := range name {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("!#$%&'*+-.^_"+string(rune(96))+"|~", char)) {
				return fmt.Errorf("invalid HTTP header name %q", name)
			}
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("HTTP header %q appears more than once with different casing", name)
		}
		seen[key] = true
		for _, char := range []byte(headers[name]) {
			if char == 127 || char < 32 && char != '\t' {
				return fmt.Errorf("invalid control character in HTTP header %q", name)
			}
		}
	}
	return nil
}
