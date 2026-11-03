package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// OAuthConfig configures client-managed upstream authorization. It is allowed
// only in canonical server configuration and agentcfg serverOverrides, never
// in portable Agent Plugins mcp.json. Secrets and tokens are not stored here.
type OAuthConfig struct {
	Account             string   `json:"account,omitempty"`
	ClientID            string   `json:"clientId,omitempty"`
	Issuer              string   `json:"issuer,omitempty"`
	ClientSecretEnv     string   `json:"clientSecretEnv,omitempty"`
	ClientMetadataURL   string   `json:"clientMetadataUrl,omitempty"`
	RedirectURL         string   `json:"redirectUrl"`
	Scopes              []string `json:"scopes,omitempty"`
	DynamicRegistration bool     `json:"dynamicRegistration,omitempty"`
}

func parseOAuth(raw json.RawMessage) (*OAuthConfig, error) {
	fields, err := object(raw)
	if err != nil {
		return nil, fmt.Errorf("oauth must be an object: %w", err)
	}
	if err = allowFields(fields, "account", "clientId", "issuer", "clientSecretEnv", "clientMetadataUrl", "redirectUrl", "scopes", "dynamicRegistration"); err != nil {
		return nil, fmt.Errorf("oauth: %w", err)
	}
	var value OAuthConfig
	for _, field := range []struct {
		name   string
		target *string
	}{
		{"account", &value.Account}, {"clientId", &value.ClientID}, {"issuer", &value.Issuer},
		{"clientSecretEnv", &value.ClientSecretEnv}, {"clientMetadataUrl", &value.ClientMetadataURL},
		{"redirectUrl", &value.RedirectURL},
	} {
		*field.target, err = stringField(fields, field.name, field.name == "redirectUrl")
		if err != nil {
			return nil, fmt.Errorf("oauth: %w", err)
		}
	}
	value.Scopes, err = stringList(fields["scopes"], "oauth.scopes")
	if err != nil {
		return nil, err
	}
	if raw, exists := fields["dynamicRegistration"]; exists {
		if string(raw) == "null" || json.Unmarshal(raw, &value.DynamicRegistration) != nil {
			return nil, fmt.Errorf("oauth.dynamicRegistration must be a boolean")
		}
	}
	if err = value.Validate(); err != nil {
		return nil, err
	}
	return &value, nil
}

// Validate also protects callers constructing Server values directly.
func (value OAuthConfig) Validate() error {
	if len(value.Account) > 128 || strings.IndexFunc(value.Account, unicode.IsControl) >= 0 || value.Account != strings.TrimSpace(value.Account) {
		return fmt.Errorf("oauth.account must be a label of at most 128 bytes without leading/trailing whitespace or controls")
	}
	if err := validateRedirectURL(value.RedirectURL); err != nil {
		return err
	}
	if value.ClientID == "" && value.ClientMetadataURL == "" && !value.DynamicRegistration {
		return fmt.Errorf("oauth requires clientId, clientMetadataUrl, or explicit dynamicRegistration:true")
	}
	if value.ClientID != "" && strings.TrimSpace(value.ClientID) == "" {
		return fmt.Errorf("oauth.clientId must not be blank")
	}
	if strings.IndexFunc(value.ClientID, unicode.IsControl) >= 0 {
		return fmt.Errorf("oauth.clientId contains control characters")
	}
	if value.ClientSecretEnv != "" {
		if value.ClientID == "" || value.Issuer == "" {
			return fmt.Errorf("oauth.clientSecretEnv requires clientId and a pinned issuer")
		}
		if strings.IndexFunc(value.ClientSecretEnv, func(r rune) bool {
			return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
		}) >= 0 {
			return fmt.Errorf("oauth.clientSecretEnv must be an environment variable name")
		}
	}
	if value.Issuer != "" {
		if value.ClientID == "" {
			return fmt.Errorf("oauth.issuer requires a preregistered clientId")
		}
		if err := validateURL(value.Issuer); err != nil {
			return fmt.Errorf("invalid oauth issuer: %w", err)
		}
		parsed, _ := url.Parse(value.Issuer)
		if parsed.RawQuery != "" || parsed.ForceQuery {
			return fmt.Errorf("oauth issuer must not contain a query")
		}
	}
	if value.ClientMetadataURL != "" {
		if err := validateURL(value.ClientMetadataURL); err != nil {
			return fmt.Errorf("invalid oauth client metadata URL: %w", err)
		}
		parsed, _ := url.Parse(value.ClientMetadataURL)
		if parsed.Scheme != "https" {
			return fmt.Errorf("oauth client metadata URL must use https")
		}
	}
	for _, scope := range value.Scopes {
		if scope == "" {
			return fmt.Errorf("oauth scopes must not be empty")
		}
		for _, char := range scope {
			if char < 0x21 || char > 0x7e || char == '"' || char == '\\' {
				return fmt.Errorf("oauth scopes must be individual printable scope tokens without whitespace, quotes or backslashes")
			}
		}
	}
	return nil
}

func validateRedirectURL(value string) error {
	if err := validateURL(value); err != nil {
		return fmt.Errorf("invalid oauth redirect URL: %w", err)
	}
	parsed, _ := url.Parse(value)
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("oauth redirect URL must not contain a query")
	}
	return nil
}

func validateServerOAuth(server Server) error {
	if server.OAuth == nil {
		return nil
	}
	if server.Type != "streamable-http" {
		return fmt.Errorf("OAuth is supported only for streamable-http upstreams")
	}
	if err := server.OAuth.Validate(); err != nil {
		return err
	}
	for name := range server.Headers {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") {
			return fmt.Errorf("OAuth cannot be combined with a literal authorization header")
		}
	}
	return nil
}
