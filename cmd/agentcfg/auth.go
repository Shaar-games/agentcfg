package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"

	"github.com/charmbracelet/huh"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// Authentication changes only the encrypted account-scoped credential store
// under the user's home. This command also supports manual callbacks; connect
// uses a client-presented or opt-in browser fetcher without reading MCP stdin.
func runAuth(ctx context.Context, home string, args []string, input io.Reader, output, diagnostics io.Writer) error {
	if len(args) < 2 || (args[0] != "login" && args[0] != "status" && args[0] != "logout") {
		return errors.New("usage: agentcfg auth <login|status|logout> <server> [--no-browser]")
	}
	operation, name := args[0], args[1]
	flags := flag.NewFlagSet("auth "+operation, flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	noBrowser := flags.Bool("no-browser", false, "print the URL instead of opening a browser")
	asJSON := flags.Bool("json", false, "show status as JSON without exposing tokens")
	plain := flags.Bool("plain", false, "plain output without Huh terminal forms")
	yes := flags.Bool("yes", false, "skip confirmation for local logout")
	if err := parse(flags, args[2:]); err != nil {
		return helpResult(err)
	}
	if (operation != "login" && *noBrowser) || (operation != "status" && *asJSON) {
		return errors.New("--no-browser is only available for login; --json is only available for status")
	}
	if operation != "logout" && *yes {
		return errors.New("--yes is only available for logout")
	}
	interactive := terminalPresentation(input, output, *plain)
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	server, found := cfg.Servers[name]
	if !found {
		return fmt.Errorf("unknown MCP server %q", name)
	}
	server.Name = name
	server = auth.InteractiveServer(server)
	if server.OAuth == nil {
		return fmt.Errorf("MCP server %q does not have OAuth configuration", name)
	}
	opts := auth.Options{Home: home}
	switch operation {
	case "status":
		state, err := auth.Inspect(ctx, server, opts)
		if err != nil {
			return err
		}
		return presentAuthStatus(ctx, input, output, name, state, interactive, *asJSON)
	case "logout":
		if interactive && !*yes {
			confirmed, err := terminalConfirm(ctx, input, output, "Forget local OAuth credentials?", "Only local encrypted records are removed. Remote tokens are not revoked.")
			if err != nil {
				return err
			}
			if !confirmed {
				return nil
			}
		}
		if err := auth.Logout(ctx, server, opts); err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "Deleted local OAuth credentials for %s (remote token revocation is not implied).\n", name)
		return err
	default:
		if input == nil {
			return errors.New("OAuth login requires interactive input")
		}
		opts.AuthorizationCodeFetcher = authorizationCodeFetcherWithForm(server.OAuth.RedirectURL, input, output, *noBrowser, interactive)
		if err := auth.Login(ctx, server, opts); err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "OAuth login completed for %s; encrypted credentials saved.\n", name)
		return err
	}
}

func presentAuthStatus(ctx context.Context, input io.Reader, output io.Writer, name string, state auth.Status, interactive, asJSON bool) error {
	// An explicit machine-readable format wins even when stdout is a terminal.
	if asJSON {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(state)
	}
	var summary strings.Builder
	fmt.Fprintf(&summary, "MCP: %s\nAccount: %s\nAuthenticated: %t\nRefresh available: %t\n", name, state.Account, state.Authenticated, state.CanRefresh)
	if !state.Expiry.IsZero() {
		fmt.Fprintf(&summary, "Expires: %s\n", state.Expiry.Format(time.RFC3339))
	}
	if len(state.Scopes) > 0 {
		fmt.Fprintf(&summary, "Scopes: %s\n", strings.Join(state.Scopes, ", "))
	}
	if interactive {
		return terminalNote(ctx, input, output, "OAuth credentials status", summary.String())
	}
	_, err := io.WriteString(output, summary.String())
	return err
}

func authorizationCodeFetcher(redirect string, input io.Reader, output io.Writer, noBrowser bool) sdkauth.AuthorizationCodeFetcher {
	return authorizationCodeFetcherWithForm(redirect, input, output, noBrowser, false)
}

func authorizationCodeFetcherWithForm(redirect string, input io.Reader, output io.Writer, noBrowser, interactive bool) sdkauth.AuthorizationCodeFetcher {
	return authorizationCodeFetcherWithOpener(redirect, input, output, noBrowser, interactive, openBrowser)
}

// A browser failure has a local, fixed diagnostic. Its cause remains available
// for classification, but is not printed: OS/openers can include callback URLs.
type oauthBrowserError struct {
	message string
	cause   error
}

func (e *oauthBrowserError) Error() string { return e.message }
func (e *oauthBrowserError) Unwrap() error { return e.cause }

// connectAuthorizationCodeFetcher handles browser interaction in the client
// process. Diagnostics must be stderr: stdin/stdout belong exclusively to MCP.
// The opener is passed explicitly so tests never launch a real browser.
func connectAuthorizationCodeFetcher(diagnostics io.Writer, opener func(string) error) func(context.Context, string, *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
	return func(ctx context.Context, redirect string, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		return fetchConnectAuthorization(ctx, args, diagnostics, authorizationCodeFetcherWithOpener(redirect, nil, diagnostics, false, false, opener))
	}
}

func connectClientAuthorizationCodeFetcher(diagnostics io.Writer) func(context.Context, string, *sdkauth.AuthorizationArgs, func(context.Context, string) error) (*sdkauth.AuthorizationResult, error) {
	return func(ctx context.Context, redirect string, args *sdkauth.AuthorizationArgs, present func(context.Context, string) error) (*sdkauth.AuthorizationResult, error) {
		return fetchConnectAuthorization(ctx, args, diagnostics, authorizationCodeFetcherWithPresentation(redirect, nil, diagnostics, false, false, present, true))
	}
}

func fetchConnectAuthorization(ctx context.Context, args *sdkauth.AuthorizationArgs, diagnostics io.Writer, fetch sdkauth.AuthorizationCodeFetcher) (*sdkauth.AuthorizationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	result, err := fetch(ctx, args)
	if err != nil && diagnostics != nil {
		message := "OAuth sign-in did not complete; retry the MCP connection or use agentcfg auth login <server>."
		var browserError *oauthBrowserError
		switch {
		case errors.Is(err, context.Canceled):
			message = "OAuth sign-in was cancelled."
		case errors.Is(err, context.DeadlineExceeded):
			message = "OAuth sign-in timed out; retry the MCP connection."
		case errors.As(err, &browserError):
			message = browserError.message
		}
		_, _ = fmt.Fprintln(diagnostics, message)
	}
	return result, err
}

func authorizationCodeFetcherWithOpener(redirect string, input io.Reader, output io.Writer, noBrowser, interactive bool, opener func(string) error) sdkauth.AuthorizationCodeFetcher {
	var present func(context.Context, string) error
	if opener != nil {
		present = func(_ context.Context, target string) error { return opener(target) }
	}
	return authorizationCodeFetcherWithPresentation(redirect, input, output, noBrowser, interactive, present, false)
}

// The callback is listening before present is called. The client may open the
// URL immediately, while the code and tokens remain outside the MCP channel.
func authorizationCodeFetcherWithPresentation(redirect string, input io.Reader, output io.Writer, noBrowser, interactive bool, present func(context.Context, string) error, inClient bool) sdkauth.AuthorizationCodeFetcher {
	if output == nil {
		output = io.Discard
	}
	return func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if args == nil || args.URL == "" {
			return nil, &oauthBrowserError{message: "OAuth provider did not return an authorization URL."}
		}
		authURL, err := url.Parse(args.URL)
		if err != nil || authURL.Hostname() == "" || authURL.User != nil || authURL.Fragment != "" || authURL.Opaque != "" ||
			(authURL.Scheme != "https" && !(authURL.Scheme == "http" && isLoopbackURL(authURL))) {
			return nil, &oauthBrowserError{message: "OAuth authorization URL must be HTTPS or loopback HTTP."}
		}
		query, err := url.ParseQuery(authURL.RawQuery)
		if err != nil || len(query["state"]) != 1 || query.Get("state") == "" {
			return nil, &oauthBrowserError{message: "OAuth authorization URL requires one nonempty state."}
		}
		redirectURL, err := url.Parse(redirect)
		if err != nil || redirectURL.Hostname() == "" || redirectURL.User != nil || redirectURL.Fragment != "" || redirectURL.Opaque != "" ||
			redirectURL.RawQuery != "" || redirectURL.ForceQuery ||
			(redirectURL.Scheme != "https" && !(redirectURL.Scheme == "http" && isLoopbackURL(redirectURL))) {
			return nil, &oauthBrowserError{message: "The configured OAuth redirect URI is invalid."}
		}
		port, portErr := strconv.Atoi(redirectURL.Port())
		loopbackCallback := isLoopbackURL(redirectURL) && redirectURL.Scheme == "http" && portErr == nil && port > 0 && port <= 65535
		if input == nil && !loopbackCallback {
			return nil, &oauthBrowserError{message: "Automatic OAuth login requires a loopback HTTP redirect with a nonzero port; use agentcfg auth login <server> for a manual callback."}
		}
		if !noBrowser && present == nil {
			if inClient {
				return nil, auth.ErrLoginRequired
			}
			return nil, &oauthBrowserError{message: "OAuth browser opener is unavailable; use agentcfg auth login <server> --no-browser."}
		}
		var server *http.Server
		var received chan string
		var serveResult chan error
		if loopbackCallback {
			var listenConfig net.ListenConfig
			listener, listenErr := listenConfig.Listen(ctx, "tcp", redirectURL.Host)
			if listenErr != nil && input == nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, &oauthBrowserError{message: "OAuth callback port is unavailable; close any other sign-in window or retry the MCP connection.", cause: listenErr}
			}
			if listenErr == nil {
				received = make(chan string, 1)
				expectedState := query.Get("state")
				path := redirectURL.EscapedPath()
				if path == "" {
					path = "/"
				}
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.EscapedPath() != path {
						http.NotFound(w, r)
						return
					}
					if r.Method != "GET" {
						http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
						return
					}
					query, err := url.ParseQuery(r.URL.RawQuery)
					if err != nil || len(query["state"]) != 1 || query.Get("state") != expectedState {
						http.Error(w, "OAuth state mismatch", http.StatusBadRequest)
						return
					}
					callback := *redirectURL
					callback.RawQuery = r.URL.RawQuery
					select {
					case received <- callback.String():
						w.Header().Set("Content-Type", "text/plain; charset=utf-8")
						_, _ = io.WriteString(w, "Authorization response received. You may return to agentcfg.")
					default:
						http.Error(w, "Authorization already received", http.StatusConflict)
					}
				})
				server = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
				serveResult = make(chan error, 1)
				go func() { serveResult <- server.Serve(listener) }()
				stopCancellation := context.AfterFunc(ctx, func() { _ = server.Close() })
				defer func() {
					stopCancellation()
					// Serve may not have registered the listener when display,
					// browser startup or cancellation fails immediately.
					defer listener.Close()
					shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					if err := server.Shutdown(shutdown); err != nil {
						_ = server.Close()
					}
				}()
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if inClient {
			_, err = fmt.Fprintln(output, "Complete OAuth sign-in in your MCP client.")
		} else if input == nil && !noBrowser {
			_, err = fmt.Fprintln(output, "Opening your browser for OAuth sign-in.")
		} else {
			_, err = fmt.Fprintf(output, "Open this authorization URL in your browser:\n%s\n", args.URL)
		}
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !noBrowser {
			if err := present(ctx, args.URL); err != nil {
				if inClient {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, auth.ErrLoginRequired) {
						return nil, err
					}
					return nil, &oauthBrowserError{message: "OAuth sign-in could not be presented by the MCP client; use agentcfg auth login <server>.", cause: err}
				}
				if input == nil {
					return nil, &oauthBrowserError{message: "OAuth browser could not be opened; use agentcfg auth login <server> --no-browser for manual sign-in.", cause: err}
				}
				if _, writeErr := fmt.Fprintln(output, "Browser could not be opened automatically. Open the URL manually."); writeErr != nil {
					return nil, writeErr
				}
			}
		}
		if received != nil {
			select {
			case callback := <-received:
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return auth.ParseAuthorizationResponse(redirect, callback)
			case err := <-serveResult:
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, &oauthBrowserError{message: "OAuth callback listener stopped before sign-in completed; retry the MCP connection.", cause: err}
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if input == nil {
			return nil, errors.New("OAuth requires an interactive callback")
		}
		if interactive {
			var response string
			err := terminalForm(ctx, input, output, huh.NewInput().
				Title("Paste the complete final redirected OAuth URL").
				Description("The authorization code is masked and not saved in shell history.").
				EchoMode(huh.EchoModePassword).
				Value(&response))
			if err != nil {
				return nil, err
			}
			return auth.ParseAuthorizationResponse(redirect, strings.TrimSpace(response))
		}
		if _, err := fmt.Fprintln(output, "Paste the complete final redirected URL here and press Enter:"); err != nil {
			return nil, err
		}
		// Match the parser's URL size limit before allocating an unbounded line.
		response, err := bufio.NewReader(io.LimitReader(input, (16<<10)+2)).ReadString('\n')
		if err != nil && len(response) == 0 {
			return nil, fmt.Errorf("read OAuth callback URL: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(response) > (16<<10)+1 {
			return nil, errors.New("OAuth response URL exceeds size limit")
		}
		return auth.ParseAuthorizationResponse(redirect, strings.TrimSpace(response))
	}
}

func isLoopbackURL(u *url.URL) bool {
	if u == nil || u.User != nil {
		return false
	}
	if strings.EqualFold(u.Hostname(), "localhost") {
		return true
	}
	address, err := netip.ParseAddr(u.Hostname())
	return err == nil && address.IsLoopback()
}

func openBrowser(rawURL string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	case "darwin":
		command = exec.Command("open", rawURL)
	default:
		command = exec.Command("xdg-open", rawURL)
	}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
