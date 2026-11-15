package auth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"sync/atomic"
	"testing"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestInteractiveExplicitUsesSavedCredentialsBeforeChallenge(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		name := "valid"
		if refresh {
			name = "expired with refresh"
		}
		t.Run(name, func(t *testing.T) {
			f := newOAuthFixture(t)
			store := testStore(t)
			f.authorize(t, store)
			if refresh {
				expireRecord(t, store, CredentialKey(f.server))
			}
			var prompts atomic.Int32
			h, err := NewInteractiveHandler(t.Context(), f.server, Options{Store: store, AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
				prompts.Add(1)
				return f.fetch(ctx, args)
			}})
			if err != nil {
				t.Fatal(err)
			}
			assertInteractiveToken(t, h)
			wantRefreshes := int32(0)
			if refresh {
				wantRefreshes = 1
			}
			if prompts.Load() != 0 || f.refreshes.Load() != wantRefreshes {
				t.Fatalf("initial credentials prompted or refreshed incorrectly: prompts=%d refreshes=%d", prompts.Load(), f.refreshes.Load())
			}
			state := h.(*interactiveHandler)
			if state.AuthorizationPending() || state.AuthorizationAttempts() != 1 {
				t.Fatalf("first credential access was not accounted for: pending=%t attempts=%d", state.AuthorizationPending(), state.AuthorizationAttempts())
			}
		})
	}
}

func TestInteractiveExplicitCredentialErrorsBeforeChallenge(t *testing.T) {
	for _, name := range []string{"missing", "expired without refresh", "corrupt", "wrong key"} {
		t.Run(name, func(t *testing.T) {
			f := newOAuthFixture(t)
			original := testStore(t)
			var store Store = original
			key := CredentialKey(f.server)
			if name != "missing" {
				f.authorize(t, store)
			}
			var want error
			switch name {
			case "expired without refresh":
				expireRecord(t, store, key)
				record, err := loadRecord(t.Context(), store, key)
				if err != nil {
					t.Fatal(err)
				}
				record.Token.RefreshToken = ""
				if err := saveRecord(t.Context(), store, key, record); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := store.Save(t.Context(), key, []byte("invalid credential record")); err != nil {
					t.Fatal(err)
				}
				want = ErrStoreCorrupt
			case "wrong key":
				var err error
				store, err = NewEncryptedFileStore(original.home, bytes.Repeat([]byte{0x32}, 32))
				if err != nil {
					t.Fatal(err)
				}
				want = ErrStoreCorrupt
			}
			var before []byte
			if name != "missing" {
				var err error
				before, err = os.ReadFile(original.filename(key, ".json"))
				if err != nil {
					t.Fatal(err)
				}
			}
			h, err := NewInteractiveHandler(t.Context(), f.server, Options{Store: store})
			if err != nil {
				t.Fatal(err)
			}
			source, err := h.TokenSource(t.Context())
			if source != nil || !errors.Is(err, want) {
				t.Fatalf("initial credential result: source=%T err=%v want=%v", source, err, want)
			}
			if name != "missing" {
				after, err := os.ReadFile(original.filename(key, ".json"))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("unusable credentials were replaced: %v", err)
				}
			}
		})
	}
}

func TestInteractiveExplicitInvalidSystemKeyDoesNotDowngrade(t *testing.T) {
	t.Setenv("AGENTCFG_OAUTH_KEY", "invalid-key")
	f := newOAuthFixture(t)
	h, err := NewInteractiveHandler(t.Context(), f.server, Options{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if source, err := h.TokenSource(t.Context()); source != nil || err == nil || needsInteractiveConsent(err) {
		t.Fatalf("invalid key was treated as anonymous access or missing consent: %T %v", source, err)
	}
}

func TestInteractiveExplicitMissingSystemKeyRemainsUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has a native secure store without an explicit key")
	}
	t.Setenv("AGENTCFG_OAUTH_KEY", "")
	if err := os.Unsetenv("AGENTCFG_OAUTH_KEY"); err != nil {
		t.Fatal(err)
	}
	f := newOAuthFixture(t)
	h, err := NewInteractiveHandler(t.Context(), f.server, Options{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if source, err := h.TokenSource(t.Context()); source != nil || !errors.Is(err, ErrSecureStoreUnavailable) {
		t.Fatalf("unavailable secure store was treated as anonymous access: %T %v", source, err)
	}
}
