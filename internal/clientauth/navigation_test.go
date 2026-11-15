package clientauth

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var continuationLink = regexp.MustCompile(`<a id="oauth-continue" href="([^"]+)">`)
var continuationRefresh = regexp.MustCompile(`<meta http-equiv="refresh" content="0;url=([^"]+)">`)

// A POST's redirect chain is governed by form-action in Chromium. A committed
// document must initiate a separate navigation before crossing to the provider
// or to a client's callback. net/http does not enforce that browser policy.
func postNavigationTarget(t *testing.T, w *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	if w.Code != http.StatusOK || w.Header().Get("Location") != "" ||
		w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("consent did not commit a navigation document: status=%d location=%q type=%q", w.Code, w.Header().Get("Location"), w.Header().Get("Content-Type"))
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "form-action 'self'") ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") ||
		w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("navigation document weakened the browser security policy")
	}
	body := w.Body.String()
	link, refresh := continuationLink.FindStringSubmatch(body), continuationRefresh.FindStringSubmatch(body)
	if len(link) != 2 || len(refresh) != 2 || html.UnescapeString(link[1]) != html.UnescapeString(refresh[1]) ||
		strings.Contains(body, "<script") || strings.Contains(body, "<form") {
		t.Fatal("navigation document did not offer matching automatic and explicit navigation")
	}
	target, err := url.Parse(html.UnescapeString(link[1]))
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		t.Fatalf("invalid continuation URL: %v", err)
	}
	return target
}

func TestClientAuthorizationConsentUsesCommittedNavigation(t *testing.T) {
	for _, scenario := range []string{"provider", "cached"} {
		t.Run(scenario, func(t *testing.T) {
			const provider = "https://provider.example/authorize?state=upstream&label=%22quoted%22"
			f := newFixture(t, func(ctx context.Context, resource Resource, principal Principal, present func(string) error, complete string) error {
				if scenario == "provider" {
					return present(provider)
				}
				return nil
			})
			c := registerClient(t, f.a)
			flow := beginFlow(t, f, c, nil)
			w := request(f.a, http.MethodPost, "/oauth/authorize", flow.values.Encode(), flow.cookie)
			target := postNavigationTarget(t, w)
			if scenario == "provider" {
				if target.String() != provider {
					t.Fatal("provider navigation changed the validated authorization URL")
				}
				return
			}
			callback, _ := url.Parse(flow.query.Get("redirect_uri"))
			if target.Scheme != callback.Scheme || target.Host != callback.Host || target.Path != callback.Path ||
				target.Query().Get("state") != flow.query.Get("state") || target.Query().Get("iss") != testIssuer || target.Query().Get("code") == "" {
				t.Fatal("cached authorization did not navigate to the bound client callback")
			}
			token := exchange(t, f.a, codeValues(flow, target.Query().Get("code")))
			if _, err := f.a.Verify(t.Context(), token.AccessToken, f.resource); err != nil {
				t.Fatal(err)
			}
		})
	}
}
