package bootstrap

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	if endpoint, err := url.Parse(value); err == nil {
		if endpoint.User != nil {
			return true
		}
		for key := range endpoint.Query() {
			if sensitiveKey(key) {
				return true
			}
		}
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
	if err := config.ValidateStdioDefinition(*server); err != nil {
		return errors.New("native process has invalid command, argument or environment data")
	}
	if !bareCommand(server.Command) {
		if !filepath.IsAbs(server.Command) {
			return errors.New("native command must name one executable; put arguments in args")
		}
		if within(root.Name(), server.Command) {
			path, err := anchoredPath(root, server.Command)
			if err != nil {
				return errors.New("native command path is unreadable or escapes its discovery location")
			}
			info, err := root.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				return errors.New("native command path must refer to a regular file")
			}
			server.Command = filepath.Join(root.Name(), path)
		}
	}
	if server.Cwd == "" {
		return nil
	}
	if !filepath.IsAbs(server.Cwd) {
		return errors.New("explicit native cwd must be an absolute directory")
	}
	if within(root.Name(), server.Cwd) {
		path, err := anchoredPath(root, server.Cwd)
		if err != nil {
			return errors.New("native cwd is unreadable or escapes its discovery location")
		}
		info, err := root.Stat(path)
		if err != nil || !info.IsDir() {
			return errors.New("native cwd must refer to a directory")
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
	if err := config.ValidateRemoteDefinition(server); err != nil {
		return errors.New("native HTTP endpoint or headers are invalid")
	}
	return nil
}

// Heuristics explain what is copied; they never decide what the user may
// import. Values stay byte-for-byte intact and never appear in these messages.
func importAdvisories(server config.Server) []string {
	sensitive, variables := secretValue(server.Command), symbolic(server.Command) || symbolic(server.Cwd)
	for _, arg := range server.Args {
		sensitive = sensitive || secretValue(arg)
		variables = variables || symbolic(arg)
		if strings.HasPrefix(arg, "-") {
			sensitive = sensitive || sensitiveKey(strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0])
		}
	}
	for key, value := range server.Env {
		sensitive = sensitive || sensitiveKey(key) || secretValue(value)
		variables = variables || symbolic(value)
	}
	sensitive = sensitive || secretValue(server.URL)
	variables = variables || symbolic(server.URL)
	for key, value := range server.Headers {
		sensitive = sensitive || sensitiveKey(key) || secretValue(value)
		variables = variables || symbolic(value)
	}
	var warnings []string
	if sensitive {
		warnings = append(warnings, "credentials are copied as configured into ~/.agentcfg/mcp.json")
	}
	if variables {
		warnings = append(warnings, "variable expressions are copied unchanged; harness-specific substitutions are not evaluated")
	}
	if strings.HasPrefix(server.URL, "http://") {
		warnings = append(warnings, "HTTP endpoint is copied as configured without TLS")
	}
	return warnings
}
