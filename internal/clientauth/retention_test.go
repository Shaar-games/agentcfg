package clientauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func restartFixtureAuthority(t *testing.T, f *fixture, now time.Time) {
	t.Helper()
	f.a.Close()
	a, err := New(Options{
		Context: t.Context(), Issuer: testIssuer, Store: f.store,
		ResolveResource: f.a.resolve, Authorize: f.a.authorize,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return now }
	f.a = a
	t.Cleanup(a.Close)
}

func TestClientAuthorizationUnusedRegistrationsExpireAcrossRestart(t *testing.T) {
	f := newFixture(t, nil)
	var first client
	for i := range maxClients {
		c := registerClient(t, f.a)
		if i == 0 {
			first = c
		}
	}
	now := f.a.now().Add(16 * time.Minute)
	restartFixtureAuthority(t, f, now)
	_ = registerClient(t, f.a)
	if _, err := f.a.registered(t.Context(), first.ID); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unused registration survived expiry/restart: %v", err)
	}
}

func TestClientAuthorizationActiveRegistrationSurvivesCleanupAndRefresh(t *testing.T) {
	f := newFixture(t, nil)
	active, token := grantTokens(t, f)
	for range maxClients - 1 {
		_ = registerClient(t, f.a)
	}
	now := f.a.now().Add(16 * time.Minute)
	restartFixtureAuthority(t, f, now)
	_ = registerClient(t, f.a)
	if _, err := f.a.registered(t.Context(), active.ID); err != nil {
		t.Fatalf("active client was collected with unused registrations: %v", err)
	}
	next := exchange(t, f.a, refreshValues(active, f.resource, token.RefreshToken))
	if _, err := f.a.Verify(t.Context(), next.AccessToken, f.resource); err != nil {
		t.Fatalf("active refresh grant was not retained: %v", err)
	}
}

func TestClientAuthorizationUnexchangedCodeReleasesPendingGrant(t *testing.T) {
	f := newFixture(t, nil)
	c := registerClient(t, f.a)
	flow := beginFlow(t, f, c, nil)
	code := consentCode(t, f, flow)
	now := f.a.now().Add(codeTTL + time.Second)
	restartFixtureAuthority(t, f, now)
	w := request(f.a, http.MethodPost, "/oauth/token", codeValues(flow, code).Encode(), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expired code accepted: %d", w.Code)
	}
	raw, err := f.store.Load(t.Context(), f.a.grantKey)
	if err != nil {
		t.Fatal(err)
	}
	var stored grantState
	if json.Unmarshal(raw, &stored) != nil || len(stored.Grants) != 0 || len(stored.Codes) != 0 {
		t.Fatalf("abandoned code retained persistent grant capacity: grants=%d codes=%d", len(stored.Grants), len(stored.Codes))
	}
}

func TestClientAuthorizationActivatedGrantOutlivesCodeTTL(t *testing.T) {
	f := newFixture(t, nil)
	c, token := grantTokens(t, f)
	now := f.a.now().Add(codeTTL + time.Second)
	restartFixtureAuthority(t, f, now)
	if _, err := f.a.Verify(t.Context(), token.AccessToken, f.resource); err != nil {
		t.Fatalf("activated grant expired with its consumed code: %v", err)
	}
	_ = exchange(t, f.a, refreshValues(c, f.resource, token.RefreshToken))
}

func TestClientAuthorizationValidConsentExtendsInactiveRegistration(t *testing.T) {
	f := newFixture(t, nil)
	c := registerClient(t, f.a)
	initial := f.a.now()
	f.a.now = func() time.Time { return initial.Add(14 * time.Minute) }
	flow := beginFlow(t, f, c, nil)
	// The browser is still inside its five-minute consent window, even though
	// registration itself is now older than the unused-client retention period.
	f.a.now = func() time.Time { return initial.Add(16 * time.Minute) }
	_ = registerClient(t, f.a) // Runs registration cleanup.
	if _, err := f.a.registered(t.Context(), c.ID); err != nil {
		t.Fatalf("live consent lost its registration: %v", err)
	}
	code := consentCode(t, f, flow)
	_ = exchange(t, f.a, codeValues(flow, code))
}

func TestClientAuthorizationRevokedClientHasReauthorizationGrace(t *testing.T) {
	f := newFixture(t, nil)
	c, token := grantTokens(t, f)
	principal, err := f.a.Verify(t.Context(), token.AccessToken, f.resource)
	if err != nil {
		t.Fatal(err)
	}
	now := f.a.now().Add(24 * time.Hour)
	f.a.now = func() time.Time { return now }
	if err := f.a.Revoke(t.Context(), principal); err != nil {
		t.Fatal(err)
	}
	_ = registerClient(t, f.a)
	if _, err := f.a.registered(t.Context(), c.ID); err != nil {
		t.Fatalf("revocation prevented immediate native reauthorization: %v", err)
	}
}

func TestClientAuthorizationInvalidResourceDoesNotKeepRegistrationAlive(t *testing.T) {
	f := newFixture(t, nil)
	c := registerClient(t, f.a)
	initial := f.a.now()
	f.a.now = func() time.Time { return initial.Add(14 * time.Minute) }
	query := authorizationQuery(c, f.resource)
	query.Set("resource", testIssuer+"/mcp/unknown")
	w := request(f.a, http.MethodGet, "/oauth/authorize?"+query.Encode(), "", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown resource accepted: %d", w.Code)
	}
	f.a.now = func() time.Time { return initial.Add(16 * time.Minute) }
	if _, err := f.a.registered(context.Background(), c.ID); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("invalid resource extended inactive registration: %v", err)
	}
}
