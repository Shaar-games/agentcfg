package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentcfg/internal/auth"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func proxyLoopbackURL(value *url.URL) bool {
	address, err := netip.ParseAddr(value.Hostname())
	return strings.EqualFold(value.Hostname(), "localhost") || err == nil && address.IsLoopback()
}

// proxyFetchAuthorization only presents a URL to an existing browser request.
// It never launches a browser and never accepts terminal/MCP input.
func proxyFetchAuthorization(ctx context.Context, redirect string, args *sdkauth.AuthorizationArgs, present func(string) error, completeURL string) (*sdkauth.AuthorizationResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if present == nil {
		return nil, auth.ErrLoginRequired
	}
	if args == nil || len(args.URL) > 16384 {
		return nil, errors.New("Invalid upstream OAuth authorization URL")
	}
	authorization, err := url.Parse(args.URL)
	if err != nil || authorization.Hostname() == "" || authorization.User != nil || authorization.Fragment != "" || authorization.Opaque != "" ||
		(authorization.Scheme != "https" && !(authorization.Scheme == "http" && proxyLoopbackURL(authorization))) {
		return nil, errors.New("Invalid upstream OAuth authorization URL")
	}
	query, err := url.ParseQuery(authorization.RawQuery)
	if err != nil || len(query["state"]) != 1 || query.Get("state") == "" {
		return nil, errors.New("Upstream OAuth state is missing")
	}
	for name := range query {
		switch strings.ToLower(name) {
		case "code", "access_token", "refresh_token", "client_secret":
			return nil, errors.New("Upstream OAuth authorization URL contains credentials")
		}
	}
	callback, err := url.Parse(redirect)
	if err != nil || callback.User != nil || callback.Fragment != "" || callback.Opaque != "" || callback.RawQuery != "" || callback.ForceQuery || callback.Scheme != "http" || !proxyLoopbackURL(callback) {
		return nil, errors.New("Native upstream OAuth requires a loopback HTTP callback; use agentcfg auth login for another configured callback")
	}
	port, err := strconv.Atoi(callback.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("Upstream OAuth callback requires a configured nonzero port")
	}
	var listen net.ListenConfig
	listener, err := listen.Listen(ctx, "tcp", callback.Host)
	if err != nil {
		return nil, errors.New("Upstream OAuth callback port is unavailable")
	}
	type outcome struct {
		value *sdkauth.AuthorizationResult
		err   error
	}
	received := make(chan outcome, 1)
	var once sync.Once
	expectedState := query.Get("state")
	path := callback.EscapedPath()
	if path == "" {
		path = "/"
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if !strings.EqualFold(r.Host, callback.Host) || r.URL.EscapedPath() != path {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if len(r.URL.String()) > 16384 {
			http.Error(w, "OAuth response exceeds the size limit", http.StatusBadRequest)
			return
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(values["state"]) != 1 || subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(expectedState)) != 1 {
			http.Error(w, "OAuth state does not match", http.StatusBadRequest)
			return
		}
		responseURL := *callback
		responseURL.RawQuery = r.URL.RawQuery
		value, parseErr := auth.ParseAuthorizationResponse(redirect, responseURL.String())
		if parseErr != nil && !values.Has("error") {
			http.Error(w, "Invalid OAuth callback", http.StatusBadRequest)
			return
		}
		accepted := false
		once.Do(func() { accepted = true; received <- outcome{value: value, err: parseErr} })
		if !accepted {
			http.Error(w, "OAuth response was already received", http.StatusConflict)
			return
		}
		// This ticket belongs to the local authority. It waits for token exchange
		// and persistence before issuing its distinct authorization code.
		http.Redirect(w, r, completeURL, http.StatusSeeOther)
	})
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	stop := context.AfterFunc(ctx, func() { _ = server.Close(); _ = listener.Close() })
	defer func() {
		stop()
		defer listener.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if server.Shutdown(cleanup) != nil {
			_ = server.Close()
		}
	}()
	if err := present(args.URL); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Upstream OAuth authorization could not be presented")
	}
	select {
	case result := <-received:
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if result.err != nil {
			return nil, errors.New("Upstream OAuth authorization was declined")
		}
		return result.value, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-finished:
		return nil, errors.New("Upstream OAuth callback stopped before completion")
	}
}
