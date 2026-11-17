package auth

import (
	"context"
	"errors"
	"runtime"
	"strings"

	"agentcfg/internal/config"
)

// SummaryState describes only locally available evidence. In particular,
// credentials_saved is not a claim that the remote server still accepts a token.
type SummaryState string

const (
	SummaryNotManaged       SummaryState = "not_managed"
	SummaryUnknown          SummaryState = "unknown"
	SummaryLoginRequired    SummaryState = "login_required"
	SummaryCredentialsSaved SummaryState = "credentials_saved"
	SummaryRefreshAvailable SummaryState = "refresh_available"
	SummaryStoreUnavailable SummaryState = "store_unavailable"
	SummaryStoreError       SummaryState = "store_error"
)

// Summary is safe for terminal output and JSON. It contains no token, account,
// upstream URL, OAuth callback URL or provider-supplied error text. LoginCommand
// is present only when local evidence establishes that a new login is needed.
type Summary struct {
	State        SummaryState `json:"state"`
	Label        string       `json:"label"`
	Message      string       `json:"message"`
	LoginCommand string       `json:"loginCommand,omitempty"`
}

// InspectSummary performs a local, read-only credential inspection. It never
// contacts a server, refreshes a token, takes a transaction lock or creates a
// credential directory. Storage failures remain visible in the returned state;
// only context cancellation interrupts the caller's inventory operation.
func InspectSummary(ctx context.Context, server config.Server, opts Options) (Summary, error) {
	_, summary, err := InspectWithSummary(ctx, server, opts)
	return summary, err
}

// InspectWithSummary derives detailed status and presentation from one local
// inspection, so a concurrent login or logout cannot split them across reads.
// Storage failures expose only the configured Enabled/Account fields and a safe
// summary; cancellation remains an error.
func InspectWithSummary(ctx context.Context, server config.Server, opts Options) (Status, Summary, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, Summary{}, err
	}
	explicit := server.OAuth != nil
	effective := InteractiveServer(server)
	if effective.OAuth == nil {
		return Status{}, Summary{State: SummaryNotManaged, Message: "OAuth login is not managed by agentcfg for this server."}, nil
	}
	state, err := Inspect(ctx, effective, opts)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return Status{}, Summary{}, contextErr
		}
		if errors.Is(err, context.Canceled) {
			return Status{}, Summary{}, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return Status{}, Summary{}, context.DeadlineExceeded
		}
		state = Status{Enabled: true, Account: effective.OAuth.Account}
		if errors.Is(err, ErrSecureStoreUnavailable) {
			return state, Summary{
				State: SummaryStoreUnavailable, Label: "credential store unavailable",
				Message: "Encrypted OAuth storage is unavailable. Configure AGENTCFG_OAUTH_KEY or a supported secure store before signing in.",
			}, nil
		}
		return state, Summary{
			State: SummaryStoreError, Label: "credential status unavailable",
			Message: "OAuth credentials could not be inspected. Check the encryption key and credential-file permissions; existing credentials were not changed.",
		}, nil
	}
	if state.LoginRequired {
		return state, loginRequiredSummary(server.Name), nil
	}
	if state.Authenticated {
		return state, Summary{
			State: SummaryCredentialsSaved, Label: "credentials saved",
			Message: "An unexpired OAuth token is stored locally. The server will verify access when this MCP connects.",
		}, nil
	}
	if state.CanRefresh {
		return state, Summary{
			State: SummaryRefreshAvailable, Label: "refresh available",
			Message: "A refresh token is stored locally. The next connection will attempt renewal; no new login is required unless renewal is rejected.",
		}, nil
	}
	if explicit || state.AuthorizationObserved || state.Stored {
		state.LoginRequired = true
		return state, loginRequiredSummary(server.Name), nil
	}
	return state, Summary{
		State: SummaryUnknown, Label: "auth unknown",
		Message: "No OAuth requirement has been recorded. Connect to determine whether this server requires a login.",
	}, nil
}

func loginRequiredSummary(name string) Summary {
	return Summary{
		State: SummaryLoginRequired, Label: "login required",
		Message:      "Sign in before connecting this MCP.",
		LoginCommand: LoginCommandFor(name),
	}
}

// LoginCommandFor produces a copyable command for a canonical server name.
// Nontrivial names use literal quoting for PowerShell on Windows and POSIX
// shells elsewhere, so spaces and shell metacharacters remain one argument.
func LoginCommandFor(name string) string { return loginCommandForOS(name, runtime.GOOS) }

func loginCommandForOS(name, goos string) string {
	safe := name != "" && strings.IndexFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
	}) < 0
	if !safe {
		if goos == "windows" {
			name = "'" + strings.ReplaceAll(name, "'", "''") + "'"
		} else {
			name = "'" + strings.ReplaceAll(name, "'", "'\\''") + "'"
		}
	}
	return "agentcfg auth login " + name
}
