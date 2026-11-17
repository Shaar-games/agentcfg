package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestManualAuthorizationRecordsRequirementWithoutDiscovery(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "implicit", true: "explicit"}[explicit], func(t *testing.T) {
			f := newOAuthFixture(t)
			if !explicit {
				f.server.OAuth = nil
			}
			original := f.server
			f.server = InteractiveServer(original)
			store := testStore(t)
			opts := Options{Store: store}
			key := CredentialKey(f.server)
			before, err := Inspect(t.Context(), f.server, opts)
			if err != nil || before.AuthorizationObserved || before.LoginRequired {
				t.Fatalf("unobserved resource reported an OAuth challenge: %+v %v", before, err)
			}
			if _, err := os.Stat(store.dir); !os.IsNotExist(err) {
				t.Fatalf("inspection created credential storage: %v", err)
			}
			h, err := NewInteractiveHandler(t.Context(), original, opts)
			if err != nil {
				t.Fatal(err)
			}
			request, response := interactiveChallenge(t, f.server.URL, "", http.StatusUnauthorized, "tools.read")
			if err := h.Authorize(t.Context(), request, response); !errors.Is(err, ErrLoginRequired) {
				t.Fatalf("manual challenge: %v", err)
			}
			status, err := Inspect(t.Context(), f.server, opts)
			if err != nil || !status.AuthorizationObserved || !status.LoginRequired || status.Stored ||
				!slices.Equal(status.RequiredScopes, []string{"tools.read"}) {
				t.Fatalf("manual challenge was not retained: %+v %v", status, err)
			}
			if f.requests.Load() != 0 || f.registrations.Load() != 0 {
				t.Fatal("manual challenge performed discovery or dynamic registration")
			}
			if _, err := store.Load(t.Context(), key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("challenge fabricated a credential record: %v", err)
			}
			if CredentialKey(f.server) != key || (original.OAuth != nil) != explicit {
				t.Fatal("challenge changed the configured identity")
			}
		})
	}
}

func TestAuthorizationRequirementFollowsRejectedGrant(t *testing.T) {
	f := newOAuthFixture(t)
	store := testStore(t)
	f.authorize(t, store)
	opts := Options{Store: store}
	h, err := NewInteractiveHandler(t.Context(), f.server, opts)
	if err != nil {
		t.Fatal(err)
	}
	request, response := interactiveChallenge(t, f.server.URL, "Bearer same-access-token", http.StatusUnauthorized, "")
	if err := h.Authorize(t.Context(), request, response); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("rejected grant did not require manual login: %v", err)
	}
	status, err := Inspect(t.Context(), f.server, opts)
	if err != nil || !status.Authenticated || !status.LoginRequired {
		t.Fatalf("locally valid but rejected grant hid login requirement: %+v %v", status, err)
	}
	if err := Login(t.Context(), f.server, Options{Store: store, AuthorizationCodeFetcher: f.fetch}); err != nil {
		t.Fatal(err)
	}
	status, err = Inspect(t.Context(), f.server, opts)
	if err != nil || status.LoginRequired || !status.Authenticated || !status.AuthorizationObserved {
		t.Fatalf("new confirmed grant did not supersede rejection: %+v %v", status, err)
	}
	// An earlier request without the newly saved token must not invalidate it.
	request, response = interactiveChallenge(t, f.server.URL, "Bearer old-access-token", http.StatusUnauthorized, "")
	if err := h.Authorize(t.Context(), request, response); err != nil {
		t.Fatal(err)
	}
	status, err = Inspect(t.Context(), f.server, opts)
	if err != nil || status.LoginRequired {
		t.Fatalf("late challenge invalidated a newer grant: %+v %v", status, err)
	}
	expireRecord(t, store, CredentialKey(f.server))
	status, err = Inspect(t.Context(), f.server, opts)
	if err != nil || status.Authenticated || !status.CanRefresh || status.LoginRequired {
		t.Fatalf("refreshable expiry was labeled manual login: %+v %v", status, err)
	}
	if err := Logout(t.Context(), f.server, opts); err != nil {
		t.Fatal(err)
	}
	status, err = Inspect(t.Context(), f.server, opts)
	if err != nil || status.Stored || !status.LoginRequired || !status.AuthorizationObserved || len(status.Scopes) != 0 || len(status.RequiredScopes) != 0 {
		t.Fatalf("logout lost known OAuth or retained credentials/scopes: %+v %v", status, err)
	}
}

func TestManualLoginRetainsUnfulfilledScopeRequirement(t *testing.T) {
	f := newOAuthFixture(t)
	f.allowWrite = true
	store := testStore(t)
	f.authorize(t, store)
	h, err := NewInteractiveHandler(t.Context(), f.server, Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	request, response := interactiveChallenge(t, f.server.URL, "Bearer same-access-token", http.StatusForbidden, "tools.write")
	if err := h.Authorize(t.Context(), request, response); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("step-up did not require manual login: %v", err)
	}
	key := CredentialKey(f.server)
	var requested []string
	err = Login(t.Context(), f.server, Options{Store: store, AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		u, err := url.Parse(args.URL)
		if err != nil {
			return nil, err
		}
		requested = append(requested, u.Query().Get("scope"))
		return f.fetch(ctx, args)
	}})
	// This provider deliberately grants only read even when write was requested.
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("partial grant was reported as a completed step-up: %v", err)
	}
	if len(requested) != 1 || !slices.Contains(strings.Fields(requested[0]), "tools.write") {
		t.Fatalf("manual login forgot the earlier tool's required scope: %v", requested)
	}
	status, err := Inspect(t.Context(), f.server, Options{Store: store})
	if err != nil || !status.Authenticated || !status.LoginRequired || !slices.Contains(status.RequiredScopes, "tools.write") {
		t.Fatalf("partial grant erased the scope requirement: %+v %v", status, err)
	}
	if CredentialKey(f.server) != key || !slices.Equal(f.server.OAuth.Scopes, []string{"tools.read"}) {
		t.Fatal("manual step-up changed canonical OAuth scopes or identity")
	}
}

func TestAuthorizationRequirementCorruptionIsNotOverwritten(t *testing.T) {
	f := newOAuthFixture(t)
	store := testStore(t)
	key := authorizationRequirementKey(CredentialKey(f.server))
	corrupt := []byte("not a valid requirement")
	if err := store.Save(t.Context(), key, corrupt); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(t.Context(), f.server, Options{Store: store}); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("inspection ignored corrupt requirement: %v", err)
	}
	h, err := NewInteractiveHandler(t.Context(), f.server, Options{Store: store, AuthorizationCodeFetcher: f.fetch})
	if err != nil {
		t.Fatal(err)
	}
	request, response := interactiveChallenge(t, f.server.URL, "", http.StatusUnauthorized, "")
	if err := h.Authorize(t.Context(), request, response); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("corrupt requirement started consent: %v", err)
	}
	after, err := store.Load(t.Context(), key)
	if err != nil || !bytes.Equal(after, corrupt) || f.requests.Load() != 0 {
		t.Fatal("corrupt requirement was replaced or caused network access")
	}
}

func TestAuthorizationChallengesKeepSDKSelectionAndScopeCase(t *testing.T) {
	for _, test := range []struct {
		name      string
		challenge string
		want      error
		scopes    []string
	}{
		{
			name:      "first Bearer scope keeps case",
			challenge: `bEaReR ERROR="insufficient_scope", Scope="Tools.Write", Bearer scope="unrelated.permission"`,
			want:      ErrLoginRequired,
			scopes:    []string{"Tools.Write"},
		},
		{
			name:      "first Bearer error prevents alternative consent",
			challenge: `Bearer error="invalid_token", Bearer error="insufficient_scope", scope="write"`,
			want:      ErrAccessDenied,
		},
		{
			name:      "Basic scope is not an OAuth scope challenge",
			challenge: `Basic realm="private", scope="write", error="insufficient_scope"`,
			want:      ErrAccessDenied,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newOAuthFixture(t)
			store := testStore(t)
			h, err := NewInteractiveHandler(t.Context(), f.server, Options{Store: store})
			if err != nil {
				t.Fatal(err)
			}
			request, response := interactiveChallenge(t, f.server.URL, "", http.StatusForbidden, "")
			response.Header.Set("WWW-Authenticate", test.challenge)
			if err := h.Authorize(t.Context(), request, response); !errors.Is(err, test.want) {
				t.Fatalf("challenge classified as %v, want %v", err, test.want)
			}
			status, err := Inspect(t.Context(), f.server, Options{Store: store})
			if err != nil || !slices.Equal(status.RequiredScopes, test.scopes) ||
				status.AuthorizationObserved != errors.Is(test.want, ErrLoginRequired) {
				t.Fatalf("challenge selected another scope or created false OAuth state: %+v %v", status, err)
			}
			if f.requests.Load() != 0 {
				t.Fatal("manual challenge performed OAuth discovery")
			}
		})
	}
}
