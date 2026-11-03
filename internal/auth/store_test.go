package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func testStore(t *testing.T) *fileStore {
	t.Helper()
	store, err := NewEncryptedFileStore(t.TempDir(), bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return store.(*fileStore)
}

func TestEncryptedStoreRoundTripAndNoPlaintext(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if _, err := store.Load(ctx, "account"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.home, ".agentcfg")); !os.IsNotExist(err) {
		t.Fatalf("read created a directory: %v", err)
	}
	secret := []byte("access-secret-and-refresh-secret")
	if err := store.Save(ctx, "account", secret); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(ctx, "account")
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("round trip: %q %v", got, err)
	}
	disk, err := os.ReadFile(store.filename("account", ".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(disk, secret) || bytes.Contains(disk, []byte(base64.StdEncoding.EncodeToString(secret))) {
		t.Fatal("plaintext credentials persisted")
	}
	entries, err := os.ReadDir(store.dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary/key files left behind: %v %v", entries, err)
	}
	if err := store.Delete(ctx, "account"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, "account"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestWrongKeyCannotReadOverwriteOrDelete(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, "account", []byte("original")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(store.filename("account", ".json"))
	wrong, err := NewEncryptedFileStore(store.home, bytes.Repeat([]byte{0x32}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wrong.Load(ctx, "account"); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatal(err)
	}
	if err = wrong.Save(ctx, "account", []byte("replacement")); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatal(err)
	}
	if err = wrong.Delete(ctx, "account"); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(store.filename("account", ".json"))
	if !bytes.Equal(before, after) {
		t.Fatal("unreadable credentials were modified")
	}
}

func TestCiphertextIsBoundToIdentityAndAuthenticated(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, "one", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(store.filename("one", ".json"))
	if err := os.WriteFile(store.filename("two", ".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, "two"); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatal(err)
	}
	var record envelope
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	record.Data[len(record.Data)-1] ^= 1
	raw, _ = json.Marshal(record)
	if err := os.WriteFile(store.filename("one", ".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, "one"); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatal(err)
	}
}

func TestCredentialStoreRejectsParentLinksAndJunctions(t *testing.T) {
	store := testStore(t)
	outside := t.TempDir()
	path := filepath.Join(store.home, ".agentcfg")
	err := os.Symlink(outside, path)
	if err != nil && runtime.GOOS == "windows" {
		err = exec.Command("cmd", "/c", "mklink", "/J", path, outside).Run()
	}
	if err != nil {
		t.Skipf("links unavailable: %v", err)
	}
	defer os.Remove(path)
	if err := store.Save(context.Background(), "account", []byte("secret")); err == nil {
		t.Fatal("followed credential parent link")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("external directory modified: %v %v", entries, err)
	}
}

func TestStoreLockCoordinatesIndependentInstancesAndCancels(t *testing.T) {
	store := testStore(t)
	other, err := NewEncryptedFileStore(store.home, bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := store.Lock(context.Background(), "account")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if second, err := other.(Locker).Lock(ctx, "account"); err == nil {
		_ = second()
		t.Fatal("second store acquired an occupied lock")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock cancellation: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	unlock, err = other.(Locker).Lock(context.Background(), "account")
	if err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitEnvironmentKeyAndInvalidKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTCFG_OAUTH_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x47}, 32)))
	store, err := NewSystemStore(home)
	if err != nil {
		t.Fatal(err)
	}
	if store.(*fileStore).protector.Scheme() != "external-aes-gcm-v1" {
		t.Fatal("explicit key ignored")
	}
	if _, err := os.Stat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
		t.Fatalf("constructor created files: %v", err)
	}
	t.Setenv("AGENTCFG_OAUTH_KEY", "bad-key-do-not-echo")
	if _, err := NewSystemStore(home); err == nil || bytes.Contains([]byte(err.Error()), []byte("bad-key-do-not-echo")) {
		t.Fatalf("invalid key not safely rejected: %v", err)
	}
}
