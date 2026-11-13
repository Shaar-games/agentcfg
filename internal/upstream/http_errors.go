package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"

	"agentcfg/internal/auth"
)

// SDK and network error text can contain URLs, credentials, certificates or
// response bytes. Classify only typed causes and numeric status codes, and
// expose fixed messages. Each sdkExchangeError belongs to a single POST.
func requestError(ctx context.Context, cause error) error {
	var network net.Error
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return errors.New("MCP HTTP request timed out; the operation is not retried")
	case ctx.Err() != nil:
		return errors.New("MCP HTTP request cancelled; remote execution may already have started")
	case errors.Is(cause, context.DeadlineExceeded) || errors.As(cause, &network) && network.Timeout():
		return errors.New("MCP HTTP request timed out; the operation is not retried")
	case errors.Is(cause, context.Canceled):
		return errors.New("MCP HTTP request cancelled; remote execution may already have started")
	}
	if errors.Is(cause, auth.ErrLoginRequired) {
		return auth.ErrLoginRequired
	}

	var oauth *oauthRequestError
	if errors.As(cause, &oauth) {
		if oauth.token {
			return errors.New("MCP OAuth token acquisition failed; check this server's credentials with 'agentcfg auth status <server>'; the operation is not retried")
		}
		return errors.New("MCP OAuth authorization failed; check this server's OAuth configuration and run 'agentcfg auth login <server>'; the operation is not retried")
	}

	var dns *net.DNSError
	var certificate *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalidCertificate x509.CertificateInvalidError
	var connection *net.OpError
	switch {
	case errors.As(cause, &dns):
		return errors.New("MCP HTTP DNS lookup failed; check the endpoint hostname and network settings; the operation is not retried")
	case errors.As(cause, &certificate), errors.As(cause, &unknownAuthority), errors.As(cause, &hostname), errors.As(cause, &invalidCertificate):
		return errors.New("MCP HTTP TLS certificate verification failed; check the certificate trust configuration; the operation is not retried")
	case errors.As(cause, &connection):
		return errors.New("MCP HTTP network connection failed; check connectivity and proxy settings; the operation is not retried")
	}

	var exchange *sdkExchangeError
	if errors.As(cause, &exchange) && exchange.status != 0 {
		status := exchange.status
		var problem, advice string
		switch status {
		case http.StatusUnauthorized:
			problem = "authentication required or credentials rejected"
			advice = "; check this server's authentication configuration in agentcfg"
		case http.StatusForbidden:
			problem = "access denied"
			advice = "; check server permissions and authorization"
		case http.StatusNotFound:
			problem = "endpoint not found"
			advice = "; check the configured MCP endpoint URL"
		case http.StatusMethodNotAllowed:
			problem = "method not allowed"
			advice = "; check that the endpoint supports MCP Streamable HTTP"
		case http.StatusProxyAuthRequired:
			problem = "proxy authentication required"
			advice = "; check the proxy credentials and network settings"
		case http.StatusTooManyRequests:
			problem = "rate limit reached"
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			problem = "redirect rejected"
			advice = "; configure the final MCP endpoint URL"
		case http.StatusAccepted, http.StatusNoContent:
			problem = "request received no JSON-RPC response"
		default:
			switch {
			case status >= 500 && status <= 599:
				problem = "server error"
			case status >= 200 && status <= 299:
				// The SDK flattens some asynchronous JSON/SSE reader errors.
				// A response was received, but its text cannot be classified safely.
				problem = "response was invalid or interrupted"
			default:
				problem = "request failed"
			}
		}
		return fmt.Errorf("MCP HTTP %s (HTTP %d)%s; the operation is not retried", problem, status, advice)
	}
	return errors.New("MCP HTTP request failed; the operation is not retried")
}

// Retain OAuth error identity for the SDK's invalid_grant handling and for
// ErrLoginRequired, while marking which local step failed without logging it.
type oauthRequestError struct {
	token bool
	err   error
}

func (e *oauthRequestError) Error() string { return "MCP OAuth request failed" }
func (e *oauthRequestError) Unwrap() error { return e.err }
