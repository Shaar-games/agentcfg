package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const DefaultGatewayListen = "127.0.0.1:43877"

func parseGatewaySettings(raw json.RawMessage, root string) (GatewaySettings, error) {
	fields, err := object(raw)
	if err != nil {
		return GatewaySettings{}, fmt.Errorf("gateway must be an object: %w", err)
	}
	if err := allowFields(fields, "enabled", "listen", "tlsCert", "tlsKey", "harnesses"); err != nil {
		return GatewaySettings{}, fmt.Errorf("gateway: %w", err)
	}
	settings := GatewaySettings{Listen: DefaultGatewayListen}
	if raw, exists := fields["harnesses"]; exists {
		if strings.TrimSpace(string(raw)) == "null" {
			return GatewaySettings{}, fmt.Errorf("gateway.harnesses must be an array of harness names")
		}
		settings.Harnesses, err = parseServerHarnesses(raw)
		if err != nil {
			return GatewaySettings{}, fmt.Errorf("gateway: %w", err)
		}
	}
	if raw, exists := fields["enabled"]; exists {
		if strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, &settings.Enabled) != nil {
			return GatewaySettings{}, fmt.Errorf("gateway.enabled must be a boolean")
		}
	}
	if _, exists := fields["listen"]; exists {
		settings.Listen, err = stringField(fields, "listen", true)
		if err != nil {
			return GatewaySettings{}, fmt.Errorf("gateway: %w", err)
		}
	}
	host, port, err := net.SplitHostPort(settings.Listen)
	address, ipErr := netip.ParseAddr(host)
	if err != nil || ipErr != nil || address.Zone() != "" || !address.IsLoopback() {
		return GatewaySettings{}, fmt.Errorf("gateway.listen must use a literal loopback IP address and a nonzero port")
	}
	if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return GatewaySettings{}, fmt.Errorf("gateway.listen must use a numeric port between 1 and 65535")
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return GatewaySettings{}, fmt.Errorf("gateway.listen must use a numeric port between 1 and 65535")
	}
	settings.Listen = net.JoinHostPort(address.String(), strconv.FormatUint(portNumber, 10))
	_, certPresent := fields["tlsCert"]
	_, keyPresent := fields["tlsKey"]
	if certPresent != keyPresent {
		return GatewaySettings{}, fmt.Errorf("gateway.tlsCert and gateway.tlsKey must be supplied together")
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"tlsCert", &settings.TLSCert}, {"tlsKey", &settings.TLSKey}} {
		if _, exists := fields[field.name]; !exists {
			continue
		}
		value, err := stringField(fields, field.name, true)
		if err != nil || strings.TrimSpace(value) == "" || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return GatewaySettings{}, fmt.Errorf("gateway.%s must be a nonempty file path without control characters", field.name)
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(root, value)
		}
		resolved, err := resolveExisting(value)
		if err != nil {
			return GatewaySettings{}, fmt.Errorf("gateway.%s: %w", field.name, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return GatewaySettings{}, fmt.Errorf("gateway.%s: %w", field.name, err)
		}
		if !info.Mode().IsRegular() {
			return GatewaySettings{}, fmt.Errorf("gateway.%s must resolve to a regular file", field.name)
		}
		*field.value = resolved
	}
	return settings, nil
}

// GatewayBaseURL returns the stable origin of validated gateway settings.
// A zero-value Listen uses the same default as settings loaded from disk.
func GatewayBaseURL(settings GatewaySettings) string {
	listen := settings.Listen
	if listen == "" {
		listen = DefaultGatewayListen
	}
	scheme := "http"
	if settings.TLSCert != "" && settings.TLSKey != "" {
		scheme = "https"
	}
	return scheme + "://" + listen
}

// GatewayForHarness permits native HTTP only for the explicitly selected path.
// Empty filters allow every named harness; a direct connection has no harness.
func GatewayForHarness(settings GatewaySettings, name string) bool {
	return settings.Enabled && (name == "" || Allows(settings.Harnesses, name))
}

// GatewayResourceURL binds a private endpoint capability to the two route names.
// An uninitialized key cannot produce a public, guessable fallback URL.
// The empty harness denotes a direct connection and uses the reserved "-" route.
func GatewayResourceURL(settings GatewaySettings, name, harness string) string {
	if len(settings.RouteKey) != 32 {
		return ""
	}
	identity, _ := json.Marshal([]string{name, harness})
	mac := hmac.New(sha256.New, settings.RouteKey)
	_, _ = mac.Write(identity)
	capability := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	encodedHarness := "-"
	if harness != "" {
		encodedHarness = base64.RawURLEncoding.EncodeToString([]byte(harness))
	}
	return GatewayBaseURL(settings) + "/mcp/" + capability + "/" + base64.RawURLEncoding.EncodeToString([]byte(name)) + "/" + encodedHarness
}
