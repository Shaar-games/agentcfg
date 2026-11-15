package clientauth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type flow struct {
	ticket    string
	cookie    string
	csrf      string
	client    client
	resource  Resource
	redirect  string
	state     string
	challenge string
	expires   time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	presented chan string
	done      chan struct{}
	err       error // written before closing done; never rendered
	started   bool  // guarded by Authority.mu
	consumed  bool  // guarded by Authority.mu
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Authorize agentcfg access</title></head>
<body><main><h1>Authorize access to {{.Resource.Name}}</h1>
<p><strong>{{.Client.Name}}</strong> requests access to this MCP server through agentcfg.</p>
<dl><dt>Client identifier</dt><dd>{{.Client.ID}}</dd>
<dt>MCP resource</dt><dd>{{.Resource.URL}}</dd>
<dt>Requested permission</dt><dd>Use this server's MCP capabilities (mcp:access).</dd>
<dt>Client callback</dt><dd>{{.Redirect}}</dd></dl>
<p>You may be asked to sign in and approve permissions with the server's provider next.</p>
<form method="post" action="/oauth/authorize">
<input type="hidden" name="ticket" value="{{.Ticket}}">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<button type="submit" name="decision" value="allow">Authorize</button>
<button type="submit" name="decision" value="deny">Deny</button>
</form></main></body></html>`))

var continuationPage = template.Must(template.New("continuation").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Continue authorization</title>
<meta http-equiv="refresh" content="0;url={{.}}"></head>
<body><main><h1>Continue authorization</h1>
<p><a id="oauth-continue" href="{{.}}">Continue</a> to finish authorizing this MCP connection.</p>
</main></body></html>`))

// Browser form-action applies to a POST's redirect chain. Commit a document
// before navigating to a previously validated provider or client callback URL,
// preserving the strict CSP and no-referrer policy on that new navigation.
func navigateBrowser(w http.ResponseWriter, r *http.Request, target string) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = continuationPage.Execute(w, target)
}

func (a *Authority) consent(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.beginConsent(w, r)
	case http.MethodPost:
		a.acceptConsent(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request")
	}
}

func (a *Authority) beginConsent(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.RawQuery) > maxBody {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	for _, values := range q {
		if len(values) != 1 {
			oauthError(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	challenge, err := base64.RawURLEncoding.DecodeString(q.Get("code_challenge"))
	if err != nil || len(challenge) != 32 || len(q.Get("code_challenge")) != 43 ||
		q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" ||
		q.Get("state") == "" || len(q.Get("state")) > 1024 || len(q.Get("client_id")) > 128 ||
		(q.Get("scope") != "" && q.Get("scope") != scopeAccess) {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	c, err := a.registered(r.Context(), q.Get("client_id"))
	if err != nil {
		a.requestFailure(w, err, "invalid_client")
		return
	}
	redirect := q.Get("redirect_uri")
	if !redirectMatches(c, redirect) {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	resource, err := a.resource(r.Context(), q.Get("resource"))
	if err != nil {
		a.requestFailure(w, err, "invalid_target")
		return
	}
	c, err = a.touchClient(r.Context(), c.ID)
	if err != nil {
		a.requestFailure(w, err, "invalid_client")
		return
	}
	ticket, err := opaque(32)
	if err != nil {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	cookie, err := opaque(32)
	if err != nil {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	csrf, err := opaque(32)
	if err != nil {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, flowTTL)
	f := &flow{
		ticket: ticket, cookie: fingerprint(cookie), csrf: fingerprint(csrf),
		client: c, resource: resource, redirect: redirect, state: q.Get("state"),
		challenge: q.Get("code_challenge"), expires: a.now().Add(flowTTL),
		ctx: ctx, cancel: cancel, presented: make(chan string, 1), done: make(chan struct{}),
	}
	a.mu.Lock()
	a.pruneFlowsLocked()
	if a.closed || len(a.flows) >= maxFlows {
		a.mu.Unlock()
		cancel()
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	a.flows[ticket] = f
	a.mu.Unlock()
	http.SetCookie(w, a.flowCookie(f, cookie, int(flowTTL.Seconds())))
	// A no-referrer form navigation sends Origin:null in browsers. Preserve the
	// exact same-origin POST check without sending a referrer to the provider.
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = consentPage.Execute(w, struct {
		Client                 client
		Resource               Resource
		Redirect, Ticket, CSRF string
	}{c, resource, redirect, ticket, csrf})
}

func (a *Authority) pruneFlowsLocked() {
	for id, f := range a.flows {
		if !a.now().Before(f.expires) {
			f.cancel()
			delete(a.flows, id)
		}
	}
}

func (a *Authority) acceptConsent(w http.ResponseWriter, r *http.Request) {
	values, err := form(w, r)
	if err != nil || r.Header.Get("Origin") != a.issuer.String() ||
		(values.Get("decision") != "allow" && values.Get("decision") != "deny") {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	a.mu.Lock()
	f := a.flows[values.Get("ticket")]
	if !a.validBrowserFlow(r, f) || f.started || f.consumed ||
		subtle.ConstantTimeCompare([]byte(f.csrf), []byte(fingerprint(values.Get("csrf")))) != 1 {
		a.mu.Unlock()
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if values.Get("decision") == "deny" {
		f.consumed = true
		delete(a.flows, f.ticket)
		f.cancel()
		a.mu.Unlock()
		http.SetCookie(w, a.flowCookie(f, "", -1))
		a.redirectError(w, r, f, "access_denied")
		return
	}
	if a.closed {
		a.mu.Unlock()
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	f.started = true
	a.wg.Add(1)
	a.mu.Unlock()
	go a.runAuthorization(f)
	// The request context only bounds this HTTP response. Browser navigation
	// must not cancel the independent, bounded authorization transaction.
	select {
	case next := <-f.presented:
		navigateBrowser(w, r, next)
	case <-f.done:
		// A very fast callback can finish after present but before this select.
		// Prefer its browser URL if one was supplied.
		select {
		case next := <-f.presented:
			navigateBrowser(w, r, next)
		default:
			a.finish(w, r, f)
		}
	case <-f.ctx.Done():
		a.finish(w, r, f)
	case <-r.Context().Done():
		return
	}
}

func (a *Authority) runAuthorization(f *flow) {
	defer a.wg.Done()
	var presented atomic.Bool
	present := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil || len(raw) > maxBody || u.Host == "" || u.User != nil ||
			u.Fragment != "" || (u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname()))) {
			return errors.New("invalid upstream authorization URL")
		}
		if !presented.CompareAndSwap(false, true) {
			return errors.New("upstream authorization URL already presented")
		}
		select {
		case <-f.ctx.Done():
			return f.ctx.Err()
		case f.presented <- raw:
			return nil
		}
	}
	principal := Principal{ClientID: f.client.ID, Resource: f.resource.URL}
	f.err = a.authorize(f.ctx, f.resource, principal, present, a.endpoint("/oauth/complete")+"?ticket="+url.QueryEscape(f.ticket))
	close(f.done)
}

func (a *Authority) complete(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != 1 || len(q["ticket"]) != 1 {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	a.mu.Lock()
	f := a.flows[q.Get("ticket")]
	valid := a.validBrowserFlow(r, f) && f.started && !f.consumed
	a.mu.Unlock()
	if !valid {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	select {
	case <-f.done:
		a.finish(w, r, f)
	case <-f.ctx.Done():
		a.finish(w, r, f)
	case <-r.Context().Done():
		return
	}
}

func (a *Authority) validBrowserFlow(r *http.Request, f *flow) bool {
	if f == nil || !a.now().Before(f.expires) || a.closed {
		return false
	}
	cookies := r.CookiesNamed(a.cookieName(f))
	return len(cookies) == 1 &&
		subtle.ConstantTimeCompare([]byte(f.cookie), []byte(fingerprint(cookies[0].Value))) == 1
}

func (a *Authority) finish(w http.ResponseWriter, r *http.Request, f *flow) {
	a.mu.Lock()
	if a.flows[f.ticket] != f || !a.validBrowserFlow(r, f) || f.consumed {
		a.mu.Unlock()
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	f.consumed = true
	delete(a.flows, f.ticket)
	a.mu.Unlock()
	defer f.cancel()
	http.SetCookie(w, a.flowCookie(f, "", -1))
	select {
	case <-f.done:
		if f.err != nil {
			a.redirectError(w, r, f, "access_denied")
			return
		}
	default:
		a.redirectError(w, r, f, "access_denied")
		return
	}
	if f.ctx.Err() != nil {
		a.redirectError(w, r, f, "access_denied")
		return
	}
	current, err := a.resource(r.Context(), f.resource.URL)
	if err != nil || current.Identity != f.resource.Identity {
		a.redirectError(w, r, f, "access_denied")
		return
	}
	code, err := a.issueCode(r.Context(), f)
	if err != nil {
		a.redirectError(w, r, f, "server_error")
		return
	}
	target, _ := url.Parse(f.redirect)
	q := target.Query()
	q.Set("code", code)
	q.Set("state", f.state)
	q.Set("iss", a.issuer.String())
	target.RawQuery = q.Encode()
	navigateBrowser(w, r, target.String())
}

func (a *Authority) cookieName(f *flow) string {
	prefix := "agentcfg_oauth_"
	if a.issuer.Scheme == "https" {
		prefix = "__Host-agentcfg_oauth_"
	}
	return prefix + f.ticket[:16]
}

func (a *Authority) flowCookie(f *flow, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: a.cookieName(f), Value: value, Path: "/", HttpOnly: true,
		Secure: a.issuer.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: maxAge,
	}
}

func (a *Authority) redirectError(w http.ResponseWriter, r *http.Request, f *flow, code string) {
	target, _ := url.Parse(f.redirect)
	q := target.Query()
	q.Set("error", code)
	q.Set("state", f.state)
	q.Set("iss", a.issuer.String())
	target.RawQuery = q.Encode()
	navigateBrowser(w, r, target.String())
}

func (a *Authority) requestFailure(w http.ResponseWriter, err error, code string) {
	if errors.Is(err, ErrUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	oauthError(w, http.StatusBadRequest, code)
}

func validVerifier(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("-._~", r))
	}) < 0
}
