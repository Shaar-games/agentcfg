package auth

import (
	"context"
	"errors"
	"testing"
)

func TestNonInteractiveHandlerObservesLoginAfterConstruction(t *testing.T) {
	f := newOAuthFixture(t)
	store := testStore(t)
	ctx := context.Background()
	h, err := NewHandler(ctx, f.server, Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	source, err := h.TokenSource(ctx)
	if err != nil || source == nil {
		t.Fatalf("missing persistent token source: %v", err)
	}
	if _, err := source.Token(); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("missing grant did not require login: %v", err)
	}
	if f.requests.Load() != 0 {
		t.Fatal("missing grant triggered network activity")
	}
	// The broker may already be running when a separate login command saves
	// the first grant. It must discover that grant without being restarted.
	f.authorize(t, store)
	requests := f.requests.Load()
	for attempt := 0; attempt < 2; attempt++ {
		token, err := source.Token()
		if err != nil || token == nil || token.AccessToken != "same-access-token" {
			t.Fatalf("login %d not observed: token=%v err=%v", attempt+1, token, err)
		}
		if f.requests.Load() != requests {
			t.Fatal("loading a cached grant unexpectedly contacted the provider")
		}
		if err := Logout(ctx, f.server, Options{Store: store}); err != nil {
			t.Fatal(err)
		}
		if _, err := source.Token(); !errors.Is(err, ErrLoginRequired) {
			t.Fatalf("logout %d not observed: %v", attempt+1, err)
		}
		if attempt == 0 {
			f.authorize(t, store)
			requests = f.requests.Load()
		}
	}
}
