package config

// Config is the resolved, validated canonical configuration of one source root.
type Config struct {
	Root     string
	Servers  map[string]Server
	Skills   []Skill
	Settings Settings
	Warnings []string
}

// Server is resolved before execution. Plugin paths and variables are expanded
// by discovery, never by a shell. Name is the stable identifier in a harness.
type Server struct {
	Name      string            `json:"-"`
	Type      string            `json:"type"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Harnesses []string          `json:"harnesses,omitempty"`
	Sharing   string            `json:"sharing,omitempty"`
	OAuth     *OAuthConfig      `json:"oauth,omitempty"`
	Source    string            `json:"-"`
}

type Skill struct {
	Name      string
	Path      string
	File      string // resolved SKILL.md binding, including explicit source links
	Harnesses []string
}

type Settings struct {
	Harnesses       []string            `json:"harnesses"`
	Runtime         Runtime             `json:"runtime,omitempty"`
	OAuth           OAuthSettings       `json:"oauth,omitempty"`
	Gateway         GatewaySettings     `json:"gateway,omitempty"`
	ServerOverrides map[string]Override `json:"serverOverrides,omitempty"`
}

// OAuthSettings controls the optional browser fallback from connect.
// Explicit auth login commands remain independent of this opt-in preference.
type OAuthSettings struct {
	AutoOpenBrowser bool `json:"autoOpenBrowser,omitempty"`
}

// GatewaySettings controls the explicitly enabled client-facing HTTP gateway.
// It never changes the canonical upstream URL or its OAuth identity.
type GatewaySettings struct {
	Enabled   bool     `json:"enabled,omitempty"`
	Listen    string   `json:"listen,omitempty"`
	TLSCert   string   `json:"tlsCert,omitempty"`
	TLSKey    string   `json:"tlsKey,omitempty"`
	Harnesses []string `json:"harnesses,omitempty"`
	// RouteKey is loaded explicitly from the encrypted store, never canonical JSON.
	RouteKey []byte `json:"-"`
}

type Runtime struct {
	IdleTimeout    string `json:"idleTimeout,omitempty"`
	RequestTimeout string `json:"requestTimeout,omitempty"`
}

type Override struct {
	Harnesses []string     `json:"harnesses,omitempty"`
	Sharing   string       `json:"sharing,omitempty"`
	OAuth     *OAuthConfig `json:"oauth,omitempty"`
}

// Allows reports whether an item is enabled for the named harness. An empty
// harness denotes a direct CLI connection; an empty filter means all harnesses.
// A "none" filter disables every named harness, including when an invalid
// programmatically constructed filter also contains other names.
func Allows(filter []string, harness string) bool {
	if len(filter) == 0 || harness == "" {
		return true
	}
	allowed := false
	for _, value := range filter {
		if value == "none" {
			return false
		}
		if value == harness {
			allowed = true
		}
	}
	return allowed
}
