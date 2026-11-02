package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"
)

// The official SDK adds resource to authorization/code-exchange requests, but
// oauth2.Config.TokenSource has no refresh-form option. This small transport
// hook preserves the same SDK-discovered resource during official oauth2
// refreshes; it never sends the parameter or credentials to another endpoint.
func withRefreshResource(ctx context.Context, tokenURL, resource string) (context.Context, error) {
	base, ok := ctx.Value(oauth2.HTTPClient).(*http.Client)
	if !ok || base == nil || base.Transport == nil {
		return nil, fmt.Errorf("OAuth refresh has no dedicated HTTP client")
	}
	client := *base
	client.Transport = &refreshResourceTransport{next: base.Transport, tokenURL: tokenURL, resource: resource}
	return context.WithValue(ctx, oauth2.HTTPClient, &client), nil
}

type refreshResourceTransport struct {
	next     http.RoundTripper
	tokenURL string
	resource string
}

func (t *refreshResourceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodPost || request.URL.String() != t.tokenURL ||
		request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || request.Body == nil {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, fmt.Errorf("unexpected OAuth refresh request")
	}
	defer request.Body.Close()
	body, err := io.ReadAll(io.LimitReader(request.Body, maxCredentialBytes+1))
	if err != nil {
		return nil, err
	}
	defer clear(body)
	if len(body) > maxCredentialBytes {
		return nil, fmt.Errorf("OAuth refresh form exceeds size limit")
	}
	form, err := url.ParseQuery(string(body))
	if err != nil || form.Get("grant_type") != "refresh_token" {
		return nil, fmt.Errorf("invalid OAuth refresh form")
	}
	if values := form["resource"]; len(values) > 1 || len(values) == 1 && values[0] != t.resource {
		return nil, fmt.Errorf("OAuth refresh changed resource")
	}
	form.Set("resource", t.resource)
	encoded := form.Encode()
	copy := request.Clone(request.Context())
	copy.Header = request.Header.Clone()
	copy.Header.Del("Content-Length")
	copy.Body = io.NopCloser(strings.NewReader(encoded))
	copy.ContentLength = int64(len(encoded))
	copy.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(encoded)), nil }
	return t.next.RoundTrip(copy)
}
