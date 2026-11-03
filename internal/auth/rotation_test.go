package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestPreregisteredClientSecretRotationUsesCurrentEnvironment(t *testing.T) {
	const variable = "AGENTCFG_TEST_CLIENT_SECRET"
	t.Setenv(variable, "old-client-secret")
	f := newOAuthFixture(t)
	f.server.OAuth.ClientSecretEnv = variable
	f.server.OAuth.DynamicRegistration = true
	store := testStore(t)
	f.authorize(t, store)
	expireRecord(t, store, CredentialKey(f.server))
	t.Setenv(variable, "rotated-client-secret")
	restored, err := NewHandler(context.Background(), f.server, Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	source, err := restored.TokenSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Token(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := f.refreshSecret
	f.mu.Unlock()
	if got != "rotated-client-secret" {
		t.Fatal("refresh did not use current client secret")
	}
	record, err := loadRecord(context.Background(), store, CredentialKey(f.server))
	if err != nil || record.Config.ClientSecret != "rotated-client-secret" {
		t.Fatalf("rotated secret not encrypted with refreshed record: %v", err)
	}
	if f.registrations.Load() != 0 {
		t.Fatal("explicit client ID unexpectedly used DCR")
	}
}

func TestSecretRotationRequiresStoredIssuerProvenance(t *testing.T) {
	const variable = "AGENTCFG_TEST_CLIENT_SECRET"
	t.Setenv(variable, "client-secret")
	f := newOAuthFixture(t)
	f.server.OAuth.ClientSecretEnv = variable
	store := testStore(t)
	h := f.authorize(t, store)
	key := CredentialKey(f.server)
	expireRecord(t, store, key)
	record, err := loadRecord(context.Background(), store, key)
	if err != nil {
		t.Fatal(err)
	}
	record.Issuer = "https://different.example.test"
	if err = saveRecord(context.Background(), store, key, record); err != nil {
		t.Fatal(err)
	}
	source, err := h.TokenSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Token(); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("mismatched issuer accepted for secret refresh: %v", err)
	}
	if f.refreshes.Load() != 0 {
		t.Fatal("secret was sent despite mismatched issuer provenance")
	}
}

func TestPinnedClientCannotFallBackToAnotherIssuersDCR(t *testing.T) {
	const variable = "AGENTCFG_TEST_CLIENT_SECRET"
	t.Setenv(variable, "client-secret")
	f := newOAuthFixture(t)
	f.server.OAuth.ClientSecretEnv = variable
	f.server.OAuth.DynamicRegistration = true
	f.server.OAuth.Issuer = "https://pinned.example.test"
	h, err := NewHandler(context.Background(), f.server, Options{Store: testStore(t), AuthorizationCodeFetcher: f.fetch})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, f.server.URL, nil)
	response, err := f.http.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Authorize(context.Background(), request, response); err == nil {
		t.Fatal("pinned client accepted different issuer")
	}
	if f.registrations.Load() != 0 {
		t.Fatal("pinned identity fell back to DCR")
	}
}
