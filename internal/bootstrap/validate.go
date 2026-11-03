package bootstrap

import (
	"errors"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"agentcfg/internal/config"
)

var nativeExpansion = regexp.MustCompile(`\$\{|\$[A-Za-z_]|\$\(|%[A-Za-z_][A-Za-z_0-9]*%|\{(?:env|file|input):`)
var credentialAssignment = regexp.MustCompile(`(?i)(?:api[-_]?key|access[-_]?key|token|secret|password|passwd|authorization|cookie)["']?\s*[:=]`)

func validName(value string) bool {
	return strings.TrimSpace(value) != "" && strings.IndexFunc(value, unicode.IsControl) == -1
}

func symbolic(value string) bool {
	return nativeExpansion.MatchString(value) || value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, "~\\")
}

func sensitiveKey(value string) bool {
	key := strings.ToUpper(strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(value))
	for _, part := range []string{"SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "AUTH", "COOKIE", "PRIVATEKEY", "APIKEY", "ACCESSKEY", "BEARER", "TOKEN", "KEYFILE"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	return key == "KEY" || strings.HasSuffix(key, "PAT")
}

func secretValue(value string) bool {
	if credentialAssignment.MatchString(value) {
		return true
	}
	if endpoint, err := url.Parse(value); err == nil && endpoint.User != nil {
		return true
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"authorization:", "authorization=", "bearer ", "basic ", "-----begin", "github_pat_", "ghp_", "sk-proj-", "sk-ant-"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	if strings.HasPrefix(value, "sk-") || strings.HasPrefix(value, "AKIA") {
		return true
	}
	if strings.HasPrefix(value, "eyJ") && strings.Count(value, ".") == 2 {
		return true
	}
	if i := strings.IndexByte(value, '='); i >= 0 && sensitiveKey(strings.TrimLeft(value[:i], "-")) {
		return true
	}
	return false
}

func validateProcess(root *os.Root, server *config.Server) error {
	fail := func() error {
		return errors.New("native process contains unsafe credentials, interpolation or ambiguous paths; manual import required")
	}
	if server.Command == "" || symbolic(server.Command) || secretValue(server.Command) || strings.ContainsRune(server.Command, 0) {
		return fail()
	}
	if !bareCommand(server.Command) {
		if !filepath.IsAbs(server.Command) {
			return fail()
		}
		if within(root.Name(), server.Command) {
			path, err := anchoredPath(root, server.Command)
			if err != nil {
				return fail()
			}
			info, err := root.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				return fail()
			}
			server.Command = filepath.Join(root.Name(), path)
		}
	}
	for _, arg := range server.Args {
		if symbolic(arg) || secretValue(arg) || strings.ContainsRune(arg, 0) {
			return fail()
		}
		if strings.HasPrefix(arg, "-") {
			flag := strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0]
			if sensitiveKey(flag) {
				return fail()
			}
		}
	}
	seen := map[string]bool{}
	for key, value := range server.Env {
		folded := strings.ToUpper(key)
		if key == "" || strings.ContainsAny(key, "=\x00") || seen[folded] ||
			strings.IndexFunc(key, unicode.IsControl) >= 0 ||
			sensitiveKey(key) || symbolic(value) || secretValue(value) || strings.ContainsRune(value, 0) {
			return fail()
		}
		seen[folded] = true
	}
	if server.Cwd == "" {
		server.Cwd = root.Name()
	}
	if !filepath.IsAbs(server.Cwd) || symbolic(server.Cwd) || strings.ContainsRune(server.Cwd, 0) {
		return fail()
	}
	if within(root.Name(), server.Cwd) {
		path, err := anchoredPath(root, server.Cwd)
		if err != nil {
			return fail()
		}
		info, err := root.Stat(path)
		if err != nil || !info.IsDir() {
			return fail()
		}
		server.Cwd = filepath.Join(root.Name(), path)
	}
	return nil
}

func bareCommand(command string) bool {
	return command != "." && command != ".." && !strings.ContainsAny(command, "/\\:") &&
		strings.IndexFunc(command, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func validateRemote(server config.Server) error {
	fail := func() error {
		return errors.New("native HTTP endpoint or headers contain unsafe credentials, interpolation or unsupported values; manual import required")
	}
	if symbolic(server.URL) || secretValue(server.URL) {
		return fail()
	}
	endpoint, err := url.Parse(server.URL)
	if err != nil || endpoint.Opaque != "" || endpoint.Hostname() == "" ||
		(endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || strings.ContainsAny(server.URL, "?#\x00") ||
		strings.ContainsAny(endpoint.Host, "\\ \t\r\n") {
		return fail()
	}
	ip, ipErr := netip.ParseAddr(endpoint.Hostname())
	if strings.Contains(endpoint.Hostname(), ":") && !strings.HasPrefix(endpoint.Host, "[") {
		return fail()
	}
	if strings.HasPrefix(endpoint.Host, "[") && (ipErr != nil || !ip.Is6()) {
		return fail()
	}
	if port := endpoint.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fail()
		}
	} else if strings.HasSuffix(endpoint.Host, ":") {
		return fail()
	}
	if endpoint.Scheme == "http" && endpoint.Hostname() != "localhost" && (ipErr != nil || !ip.IsLoopback()) {
		return fail()
	}
	seen := map[string]bool{}
	for name, value := range server.Headers {
		key := strings.ToLower(name)
		if name == "" || seen[key] || sensitiveKey(name) || symbolic(value) || secretValue(value) {
			return fail()
		}
		for _, c := range name {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_"+string(rune(96))+"|~", c)) {
				return fail()
			}
		}
		for _, c := range value {
			if c == 127 || c < 32 && c != '\t' {
				return fail()
			}
		}
		seen[key] = true
	}
	return nil
}
