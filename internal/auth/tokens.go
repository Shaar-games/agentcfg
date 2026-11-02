package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

type credentialRecord struct {
	Version           int           `json:"version"`
	Identity          string        `json:"identity"`
	Generation        string        `json:"generation"`
	Config            oauth2.Config `json:"config"`
	Token             oauth2.Token  `json:"token"`
	Scopes            []string      `json:"scopes,omitempty"`
	Resource          string        `json:"resource"`
	Issuer            string        `json:"issuer,omitempty"`
	RefreshInProgress bool          `json:"refreshInProgress,omitempty"`
}

func newRecord(key string, cfg *oauth2.Config, token *oauth2.Token, scopes []string, resource, issuer string) *credentialRecord {
	var nonce [16]byte
	// crypto/rand.Read is infallible on supported Go versions (panics on a
	// broken system RNG). Generation is not a secret.
	_, _ = rand.Read(nonce[:])
	return &credentialRecord{Version: 1, Identity: key, Generation: hex.EncodeToString(nonce[:]), Config: *cfg, Token: *token, Scopes: append([]string(nil), scopes...), Resource: resource, Issuer: issuer}
}

func loadRecord(ctx context.Context, store Store, key string) (*credentialRecord, error) {
	data, err := store.Load(ctx, key)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	var record credentialRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF ||
		record.Version != 1 || record.Identity != key || record.Generation == "" ||
		record.Token.AccessToken == "" || record.Config.ClientID == "" || record.Config.Endpoint.TokenURL == "" || record.Resource == "" {
		return nil, ErrStoreCorrupt
	}
	return &record, nil
}

func saveRecord(ctx context.Context, store Store, key string, record *credentialRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return safeError("encode OAuth credentials", err)
	}
	defer clear(data)
	if err = store.Save(ctx, key, data); err != nil {
		return safeError("persist OAuth credentials", err)
	}
	return nil
}

type scopeState struct {
	mu     sync.Mutex
	scopes []string
}

func (s *scopeState) get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.scopes...)
}
func (s *scopeState) set(value []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scopes = append([]string(nil), value...)
}
func unionScopes(groups ...[]string) []string {
	var result []string
	seen := map[string]bool{}
	for _, group := range groups {
		for _, scope := range group {
			if scope != "" && !seen[scope] {
				seen[scope] = true
				result = append(result, scope)
			}
		}
	}
	return result
}
func grantedScopes(token *oauth2.Token, fallback []string) []string {
	if raw, ok := token.Extra("scope").(string); ok {
		return strings.Fields(raw)
	}
	return append([]string(nil), fallback...)
}

// Every Token call reloads under the cross-process lock, so logout is effective
// for existing handlers and concurrent refreshes consume the latest rotated
// refresh token. No stale in-memory source can overwrite another process.
type tokenSource struct {
	ctx         context.Context
	store       Store
	locker      Locker
	key         string
	scopes      *scopeState
	gate        chan struct{}
	pending     *credentialRecord
	pendingBase string
	clientID    string
	issuer      string
	secretEnv   string
}

func (s *tokenSource) Token() (*oauth2.Token, error) {
	return s.token(context.Background())
}

type requestTokenSource struct {
	source *tokenSource
	ctx    context.Context
}

func (s *requestTokenSource) Token() (*oauth2.Token, error) { return s.source.token(s.ctx) }

// Preserve the SDK's HTTP-client context values, but use the current operation's
// deadline/cancellation for lock acquisition, HTTP refresh, and persistence.
type operationContext struct {
	context.Context
	base context.Context
}

func (c operationContext) Value(key any) any {
	if key == oauth2.HTTPClient {
		return c.base.Value(key)
	}
	if value := c.Context.Value(key); value != nil {
		return value
	}
	return c.base.Value(key)
}

func (s *tokenSource) token(operation context.Context) (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(operationContext{Context: operation, base: s.ctx}, 45*time.Second)
	defer cancel()
	// A Go synchronization primitive also protects in-memory rotation recovery;
	// filesystem locking alone is not a Go memory-model synchronization edge.
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	unlock, err := s.locker.Lock(ctx, s.key)
	if err != nil {
		return nil, err
	}
	defer unlock()
	record, err := loadRecord(ctx, s.store, s.key)
	if errors.Is(err, ErrNotFound) {
		s.pending = nil
		return nil, ErrLoginRequired
	}
	if err != nil {
		return nil, err
	}
	if s.pending != nil {
		// A rotation may have succeeded just before a disk-write failure.
		// Retry persisting that result before using the spent refresh token.
		// Never resurrect credentials deleted/replaced by another process.
		if record.Generation == s.pendingBase && record.RefreshInProgress {
			if err = saveRecord(ctx, s.store, s.key, s.pending); err != nil {
				return nil, err
			}
			record = s.pending
		}
		s.pending = nil
	}
	if record.RefreshInProgress {
		return nil, ErrLoginRequired
	}
	if record.Token.Valid() {
		s.scopes.set(record.Scopes)
		token := record.Token
		return &token, nil
	}
	if record.Token.RefreshToken == "" {
		return nil, ErrLoginRequired
	}
	if err := s.currentClientSecret(record); err != nil {
		return nil, err
	}
	// Persist intent BEFORE the request can consume a rotating refresh token.
	// A crash or an unsaved response must not let another process replay the
	// old token. Only this source's pending result can safely clear the marker;
	// otherwise the caller must perform a new authorization.
	marker := newRecord(s.key, &record.Config, &record.Token, record.Scopes, record.Resource, record.Issuer)
	marker.RefreshInProgress = true
	if err := saveRecord(ctx, s.store, s.key, marker); err != nil {
		return nil, err
	}
	// Absolute Expiry is retained across restarts. ExpiresIn must never be
	// reinterpreted relative to process startup.
	refreshContext, err := withRefreshResource(ctx, record.Config.Endpoint.TokenURL, record.Resource)
	if err != nil {
		return nil, err
	}
	token, err := record.Config.TokenSource(refreshContext, &record.Token).Token()
	if err != nil {
		return nil, safeError("OAuth token refresh (a new login may be required)", err)
	}
	if token == nil || token.AccessToken == "" {
		return nil, fmt.Errorf("OAuth refresh returned no access token")
	}
	next := newRecord(s.key, &record.Config, token, grantedScopes(token, record.Scopes), record.Resource, record.Issuer)
	if err = saveRecord(ctx, s.store, s.key, next); err != nil {
		s.pending, s.pendingBase = next, marker.Generation
		return nil, err
	}
	s.scopes.set(next.Scopes)
	return token, nil
}

func (s *tokenSource) currentClientSecret(record *credentialRecord) error {
	if s.secretEnv == "" {
		return nil
	}
	if s.issuer == "" || record.Issuer != s.issuer || record.Config.ClientID != s.clientID {
		return ErrLoginRequired
	}
	secret, ok := os.LookupEnv(s.secretEnv)
	if !ok || secret == "" {
		return fmt.Errorf("OAuth client secret environment variable %s is not set", s.secretEnv)
	}
	record.Config.ClientSecret = secret
	return nil
}
