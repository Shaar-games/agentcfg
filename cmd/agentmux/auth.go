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
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"agentmux/internal/auth"
	"agentmux/internal/config"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// Authentication changes only the encrypted account-scoped credential store
// under the user's home. Login is explicitly interactive and never performed
// automatically by the broker or the TUI.
func runAuth(ctx context.Context, home string, args []string, input io.Reader, output, diagnostics io.Writer) error {
	if len(args) < 2 || (args[0] != "login" && args[0] != "status" && args[0] != "logout") {
		return errors.New("usage: agentmux auth <login|status|logout> <server> [--no-browser]")
	}
	operation, name := args[0], args[1]
	flags := flag.NewFlagSet("auth "+operation, flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	noBrowser := flags.Bool("no-browser", false, "print the URL instead of opening a browser")
	asJSON := flags.Bool("json", false, "show status as JSON without exposing tokens")
	if err := parse(flags, args[2:]); err != nil {
		return helpResult(err)
	}
	if (operation != "login" && *noBrowser) || (operation != "status" && *asJSON) {
		return errors.New("--no-browser is only available for login; --json is only available for status")
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	server, found := cfg.Servers[name]
	if !found {
		return fmt.Errorf("unknown MCP server %q", name)
	}
	server.Name = name
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
		if *asJSON {
			encoded, err := json.MarshalIndent(state, "", "  ")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(output, string(encoded))
			return err
		}
		fmt.Fprintf(output, "MCP: %s\nAccount: %s\nAuthenticated: %t\nRefresh available: %t\n", name, state.Account, state.Authenticated, state.CanRefresh)
		if !state.Expiry.IsZero() {
			fmt.Fprintf(output, "Expires: %s\n", state.Expiry.Format(time.RFC3339))
		}
		if len(state.Scopes) > 0 {
			fmt.Fprintf(output, "Scopes: %s\n", strings.Join(state.Scopes, ", "))
		}
		return nil
	case "logout":
		if err := auth.Logout(ctx, server, opts); err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "Deleted local OAuth credentials for %s (remote token revocation is not implied).\n", name)
		return err
	default:
		if input == nil {
			return errors.New("OAuth login requires interactive input")
		}
		opts.AuthorizationCodeFetcher = authorizationCodeFetcher(server.OAuth.RedirectURL, input, output, *noBrowser)
		if err := auth.Login(ctx, server, opts); err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "OAuth login completed for %s; encrypted credentials saved.\n", name)
		return err
	}
}

func authorizationCodeFetcher(redirect string, input io.Reader, output io.Writer, noBrowser bool) sdkauth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		if args == nil || args.URL == "" {
			return nil, errors.New("OAuth authorization endpoint was not provided")
		}
		authURL, err := url.Parse(args.URL)
		if err != nil || authURL.User != nil || authURL.Fragment != "" || (authURL.Scheme != "https" && !isLoopbackURL(authURL)) {
			return nil, errors.New("OAuth authorization URL must be HTTPS or loopback HTTP")
		}
		redirectURL, err := url.Parse(redirect)
		if err != nil {
			return nil, fmt.Errorf("invalid configured OAuth redirect URI: %w", err)
		}
		var listener net.Listener
		var server *http.Server
		var received chan string
		if isLoopbackURL(redirectURL) && redirectURL.Scheme == "http" && redirectURL.Port() != "" {
			if listener, err = net.Listen("tcp", redirectURL.Host); err == nil {
				received = make(chan string, 1)
				expectedState := authURL.Query().Get("state")
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
					if expectedState != "" && r.URL.Query().Get("state") != expectedState {
						http.Error(w, "OAuth state mismatch", http.StatusBadRequest)
						return
					}
					callback := *redirectURL
					callback.RawQuery = r.URL.RawQuery
					select {
					case received <- callback.String():
						w.Header().Set("Content-Type", "text/plain; charset=utf-8")
						_, _ = io.WriteString(w, "Authorization response received. You may return to agentmux.")
					default:
						http.Error(w, "Authorization already received", http.StatusConflict)
					}
				})
				server = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
				go func() { _ = server.Serve(listener) }()
				defer func() {
					shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					_ = server.Shutdown(shutdown)
				}()
			}
		}
		fmt.Fprintf(output, "Open this authorization URL in your browser:\n%s\n", args.URL)
		if !noBrowser {
			if err := openBrowser(args.URL); err != nil {
				fmt.Fprintf(output, "Browser could not be opened automatically (%v). Open the URL manually.\n", err)
			}
		}
		if received != nil {
			select {
			case callback := <-received:
				return auth.ParseAuthorizationResponse(redirect, callback)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if input == nil {
			return nil, errors.New("OAuth requires an interactive callback")
		}
		fmt.Fprintln(output, "Paste the complete final redirected URL here and press Enter:")
		response, err := bufio.NewReader(input).ReadString('\n')
		if err != nil && len(response) == 0 {
			return nil, fmt.Errorf("read OAuth callback URL: %w", err)
		}
		return auth.ParseAuthorizationResponse(redirect, strings.TrimSpace(response))
	}
}

func isLoopbackURL(u *url.URL) bool {
	if u == nil || u.User != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
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
