package auth

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type failOnceStore struct {
	Store
	Locker
	fail       atomic.Bool
	failMarker atomic.Bool
}

func (s *failOnceStore) Save(ctx context.Context, key string, value []byte) error {
	var record credentialRecord
	if err := json.Unmarshal(value, &record); err != nil {
		return err
	}
	if record.RefreshInProgress && s.failMarker.CompareAndSwap(true, false) {
		return errors.New("simulated refresh-intent write failure")
	}
	if !record.RefreshInProgress && s.fail.CompareAndSwap(true, false) {
		return errors.New("simulated credential write failure")
	}
	return s.Store.Save(ctx, key, value)
}

func TestRefreshDoesNotStartWithoutPersistedIntent(t *testing.T) {
	f := newOAuthFixture(t)
	files := testStore(t)
	store := &failOnceStore{Store: files, Locker: files}
	h := f.authorize(t, store)
	expireRecord(t, store, CredentialKey(f.server))
	store.failMarker.Store(true)
	source, err := h.TokenSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Token(); err == nil {
		t.Fatal("refresh succeeded without persisted intent")
	}
	if f.refreshes.Load() != 0 {
		t.Fatal("provider consumed a refresh token before intent was saved")
	}
	record, err := loadRecord(context.Background(), files, CredentialKey(f.server))
	if err != nil || record.RefreshInProgress {
		t.Fatalf("failed intent write changed existing record: %v", err)
	}
}

func expireRecord(t *testing.T, store Store, key string) {
	t.Helper()
	record, err := loadRecord(context.Background(), store, key)
	if err != nil {
		t.Fatal(err)
	}
	record.Token.Expiry = time.Now().Add(-time.Minute)
	if err := saveRecord(context.Background(), store, key, record); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshSaveFailureRetriesPersistenceBeforeSpentToken(t *testing.T) {
	f := newOAuthFixture(t)
	files := testStore(t)
	store := &failOnceStore{Store: files, Locker: files}
	h := f.authorize(t, store)
	expireRecord(t, store, CredentialKey(f.server))
	store.fail.Store(true)
	source, err := h.TokenSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(); err == nil {
		t.Fatal("refresh reported success before credentials were persisted")
	}
	peer, err := NewHandler(context.Background(), f.server, Options{Store: files})
	if err != nil {
		t.Fatal(err)
	}
	peerSource, err := peer.TokenSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = peerSource.Token(); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("peer reused spent refresh token: %v", err)
	}
	if f.refreshes.Load() != 1 {
		t.Fatal("peer sent another refresh request after failed persistence")
	}
	status, err := Inspect(context.Background(), f.server, Options{Store: files})
	if err != nil || status.Authenticated || status.CanRefresh {
		t.Fatalf("incomplete refresh shown as usable: %+v %v", status, err)
	}
	token, err := source.Token()
	if err != nil || token == nil || token.RefreshToken != "refresh-1" {
		t.Fatalf("rotation recovery: %v %v", token, err)
	}
	if got := f.refreshes.Load(); got != 1 {
		t.Fatalf("spent refresh token retried: %d requests", got)
	}
}

func TestRefreshUsesOperationCancellation(t *testing.T) {
	f := newOAuthFixture(t)
	store := testStore(t)
	h := f.authorize(t, store)
	expireRecord(t, store, CredentialKey(f.server))
	f.refreshStarted = make(chan struct{}, 1)
	f.refreshBlock = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source, err := h.TokenSource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := source.Token(); done <- err }()
	select {
	case <-f.refreshStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not reach provider")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lost operation cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh ignored operation cancellation")
	}
}

func TestTokenWaitForLockUsesOperationCancellation(t *testing.T) {
	f := newOAuthFixture(t)
	store := testStore(t)
	h := f.authorize(t, store)
	unlock, err := store.Lock(context.Background(), CredentialKey(f.server))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	source, err := h.TokenSource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Token(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock ignored deadline: %v", err)
	}
}
