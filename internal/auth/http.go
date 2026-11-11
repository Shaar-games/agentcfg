package auth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agentcfg/internal/config"
)

func newHTTPClient(server config.Server, supplied *http.Client) (*http.Client, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	if supplied != nil {
		*client = *supplied
	}
	if client.Timeout <= 0 || client.Timeout > 45*time.Second {
		client.Timeout = 30 * time.Second
	}
	// OAuth redirects must not move client credentials or tokens to another
	// endpoint. Browser redirects are handled separately by the caller.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Jar != nil {
		return nil, fmt.Errorf("OAuth HTTP client must not use a shared cookie jar")
	}
	transport := client.Transport
	if transport == nil {
		base := http.DefaultTransport.(*http.Transport).Clone()
		// Resolve and validate addresses at dial time to prevent DNS rebinding
		// into local networks. A caller-supplied transport owns its dial policy.
		base.Proxy = nil
		base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("resolve OAuth host: %w", err)
			}
			for _, ip := range ips {
				if !trustedHost(host, server) && unsafeIP(ip.IP) {
					return nil, fmt.Errorf("OAuth endpoint resolves to a non-public address")
				}
			}
			dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
			for _, ip := range ips {
				conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if dialErr == nil {
					return conn, nil
				}
				err = dialErr
			}
			if err == nil {
				err = fmt.Errorf("OAuth host has no addresses")
			}
			return nil, err
		}
		transport = base
	}
	client.Transport = &guardedTransport{next: transport, server: server}
	return client, nil
}

type guardedTransport struct {
	next   http.RoundTripper
	server config.Server
}

func (t *guardedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL == nil {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, fmt.Errorf("OAuth request has no URL")
	}
	if err := allowedURL(request.URL.String(), t.server); err != nil {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, err
	}
	return t.next.RoundTrip(request)
}

func allowedURL(raw string, server config.Server) error {
	if len(raw) > 16384 {
		return fmt.Errorf("OAuth URL exceeds size limit")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("OAuth URL must be an absolute HTTPS URL (HTTP is allowed for configured loopback hosts)")
	}
	if u.User != nil || strings.Contains(raw, "#") || strings.ContainsAny(u.Host, "\\ \t\r\n") {
		return fmt.Errorf("OAuth URL must not contain credentials, fragments or an invalid host")
	}
	host := u.Hostname()
	addr, ipErr := netip.ParseAddr(host)
	if strings.Contains(host, ":") && !strings.HasPrefix(u.Host, "[") {
		return fmt.Errorf("OAuth IPv6 hosts must use brackets")
	}
	if strings.HasPrefix(u.Host, "[") && (ipErr != nil || !addr.Is6()) {
		return fmt.Errorf("invalid OAuth IPv6 host")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("invalid OAuth URL port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("empty OAuth URL port")
	}
	loopback := strings.EqualFold(host, "localhost") || ipErr == nil && addr.IsLoopback()
	if u.Scheme == "http" && (!loopback || !trustedHost(host, server)) {
		return fmt.Errorf("OAuth HTTP endpoints must be on an explicitly configured loopback host")
	}
	if ipErr == nil && unsafeIP(net.IP(addr.AsSlice())) && !trustedHost(host, server) {
		return fmt.Errorf("OAuth endpoint uses an unconfigured non-public address")
	}
	if strings.EqualFold(host, "localhost") && !trustedHost(host, server) {
		return fmt.Errorf("OAuth endpoint uses an unconfigured loopback host")
	}
	return nil
}

func trustedHost(host string, server config.Server) bool {
	values := []string{server.URL}
	if server.OAuth != nil {
		values = append(values, server.OAuth.Issuer)
	}
	for _, raw := range values {
		u, err := url.Parse(raw)
		if err == nil && u.Hostname() != "" && strings.EqualFold(host, u.Hostname()) {
			return true
		}
	}
	return false
}
func unsafeIP(ip net.IP) bool {
	return !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

// Resource headers are attached only to the configured MCP origin, never to
// metadata/registration/token endpoints. The OAuth client uses another chain.
type resourceTransport struct {
	next    http.RoundTripper
	origin  string
	headers map[string]string
}

func (t *resourceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme+"://"+request.URL.Host != t.origin {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, fmt.Errorf("MCP request changed origin")
	}
	copy := request.Clone(request.Context())
	copy.Header = make(http.Header, len(t.headers)+len(request.Header))
	for name, value := range t.headers {
		lower := strings.ToLower(name)
		// These values describe the current SDK request/session and cannot be
		// inherited from configuration. Match the normal MCP transport so the
		// explicit login handshake works with the same configured headers.
		if lower == "mcp-session-id" || lower == "mcp-protocol-version" ||
			lower == "mcp-method" || lower == "mcp-name" || lower == "last-event-id" ||
			strings.HasPrefix(lower, "mcp-param-") {
			continue
		}
		copy.Header.Set(name, value)
	}
	// The SDK owns negotiation/framing headers such as Accept and Content-Type.
	for name, values := range request.Header {
		copy.Header[name] = append([]string(nil), values...)
	}
	return t.next.RoundTrip(copy)
}
