// Package auth adapts the official MCP SDK OAuth handler to encrypted,
// account-scoped credential persistence. It never opens a browser or listener.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"agentmux/internal/pathutil"
	"github.com/gofrs/flock"
)

var (
	ErrNotFound               = errors.New("OAuth credentials not found")
	ErrSecureStoreUnavailable = errors.New("no native secure credential store is available; supply an explicit encrypted Store")
	ErrStoreCorrupt           = errors.New("OAuth credential store is corrupt or cannot be decrypted")
	ErrLoginRequired          = errors.New("OAuth login required: provide an AuthorizationCodeFetcher through the interactive caller")
)

const maxCredentialBytes = 1 << 20
const maxEnvelopeBytes = 2 << 20

// Store holds encrypted credentials. Keys are opaque credential identities.
// Implementations must never persist plaintext credentials or encryption keys.
// Save must be atomic and must not replace an existing unreadable record.
type Store interface {
	Load(context.Context, string) ([]byte, error)
	Save(context.Context, string, []byte) error
	Delete(context.Context, string) error
}

// Locker serializes a complete load/refresh/save transaction, including across
// processes. Stores supplied to NewHandler must also implement Locker.
// Unlock must always be called. Individual Store operations do not take this
// transaction lock, so they can be called while it is held.
type Locker interface {
	Lock(context.Context, string) (unlock func() error, err error)
}

type fileStore struct {
	home      string
	dir       string
	protector protector
}

type envelope struct {
	Version int    `json:"version"`
	Scheme  string `json:"scheme"`
	Data    []byte `json:"data"`
}

// NewSystemStore uses per-user Windows DPAPI, or an explicitly supplied
// AGENTMUX_OAUTH_KEY (base64 encoding of 32 random bytes). Other platforms without
// that key return ErrSecureStoreUnavailable instead of writing plaintext. Construction and
// missing-record reads do not create directories or access DPAPI.
// An empty home uses os.UserHomeDir; tests should always provide a temp home.
func NewSystemStore(home string) (Store, error) {
	if value, exists := os.LookupEnv("AGENTMUX_OAUTH_KEY"); exists {
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(key) != 32 {
			clear(key)
			return nil, fmt.Errorf("AGENTMUX_OAUTH_KEY must be base64 encoding of exactly 32 random bytes")
		}
		defer clear(key)
		return NewEncryptedFileStore(home, key)
	}
	p, err := systemProtector()
	if err != nil {
		return nil, err
	}
	return newFileStore(home, p)
}

// NewEncryptedFileStore is an explicit alternative for callers that obtain a
// 32-byte key from their own secure key manager. The key is copied into memory
// and is never written to disk or derived from a machine/user identifier.
func NewEncryptedFileStore(home string, key []byte) (Store, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("credential encryption requires a 32-byte external key")
	}
	return newFileStore(home, &aesProtector{key: append([]byte(nil), key...)})
}

func newFileStore(home string, p protector) (*fileStore, error) {
	var err error
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate credential home: %w", err)
		}
	}
	home, err = pathutil.Resolve(home)
	if err != nil {
		return nil, fmt.Errorf("resolve credential home: %w", err)
	}
	info, err := os.Stat(home)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("credential home must be an existing directory")
	}
	return &fileStore{home: home, dir: filepath.Join(home, ".agentmux", "credentials", "oauth"), protector: p}, nil
}

func (s *fileStore) filename(key, suffix string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+suffix)
}

func (s *fileStore) Load(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.ensureDir(false); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	path := s.filename(key, ".json")
	info, err := s.checkPath(path, false)
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("credential record must be a regular file")
	}
	if err := checkPrivatePath(path, info); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read credential record: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("credential file changed during open")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxEnvelopeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read credential record: %w", err)
	}
	if len(data) > maxEnvelopeBytes {
		return nil, ErrStoreCorrupt
	}
	var record envelope
	if json.Unmarshal(data, &record) != nil || record.Version != 1 || record.Scheme != s.protector.Scheme() || len(record.Data) == 0 {
		return nil, ErrStoreCorrupt
	}
	plain, err := s.protector.Open(key, record.Data)
	if err != nil {
		return nil, ErrStoreCorrupt
	}
	if len(plain) == 0 || len(plain) > maxCredentialBytes {
		clear(plain)
		return nil, ErrStoreCorrupt
	}
	return plain, nil
}

func (s *fileStore) Save(ctx context.Context, key string, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(value) == 0 || len(value) > maxCredentialBytes {
		return fmt.Errorf("credential record has invalid size")
	}
	// Do not silently replace a record encrypted by another key/provider, or a
	// corrupt record. Recovery must be an explicit caller decision.
	existing, err := s.Load(ctx, key)
	clear(existing)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	sealed, err := s.protector.Seal(key, value)
	if err != nil {
		return fmt.Errorf("encrypt credential record: %w", err)
	}
	data, err := json.Marshal(envelope{Version: 1, Scheme: s.protector.Scheme(), Data: sealed})
	if err != nil {
		return err
	}
	if len(data) > maxEnvelopeBytes {
		return fmt.Errorf("encrypted credential record exceeds size limit")
	}
	if err = s.ensureDir(true); err != nil {
		return err
	}
	path := s.filename(key, ".json")
	if _, err = s.checkPath(path, true); err != nil {
		return err
	}
	file, err := os.CreateTemp(s.dir, ".credential-")
	if err != nil {
		return fmt.Errorf("create encrypted credential record: %w", err)
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err = privatePath(temp, false); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("write encrypted credential record: %w", err)
	}
	if closeErr != nil {
		return closeErr
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = s.ensureDir(false); err != nil {
		return err
	}
	if _, err = s.checkPath(path, true); err != nil {
		return err
	}
	if err = os.Rename(temp, path); err != nil {
		return fmt.Errorf("replace encrypted credential record: %w", err)
	}
	return nil
}

func (s *fileStore) Delete(ctx context.Context, key string) error {
	value, err := s.Load(ctx, key)
	clear(value)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	path := s.filename(key, ".json")
	if _, err = s.checkPath(path, false); err != nil {
		return err
	}
	return os.Remove(path)
}

func (s *fileStore) Lock(ctx context.Context, key string) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.ensureDir(true); err != nil {
		return nil, err
	}
	path := s.filename(key, ".lock")
	if _, err := s.checkPath(path, true); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	err = privatePath(path, false)
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	lock := flock.New(path)
	ok, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ctx.Err()
	}
	if _, err := s.checkPath(path, false); err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	return lock.Unlock, nil
}

func (s *fileStore) ensureDir(create bool) error {
	path := s.home
	for _, name := range []string{".agentmux", "credentials", "oauth"} {
		path = filepath.Join(path, name)
		info, err := s.checkPath(path, true)
		if err != nil {
			return err
		}
		if info == nil {
			if !create {
				return os.ErrNotExist
			}
			if err = os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
				return err
			}
			if _, err = s.checkPath(path, false); err != nil {
				return err
			}
			if err = privatePath(path, true); err != nil {
				return err
			}
			info, err = os.Stat(path)
			if err != nil {
				return err
			}
		}
		if !info.IsDir() {
			return fmt.Errorf("credential parent must be a directory")
		}
		if err = checkPrivatePath(path, info); err != nil {
			return err
		}
	}
	return nil
}

// Every component is checked against a physically resolved home. In
// particular, Windows junctions are resolved using pathutil's handle-based
// implementation; filepath.EvalSymlinks alone is insufficient there.
func (s *fileStore) checkPath(path string, missingOK bool) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && missingOK {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("credential paths must not be symbolic links")
	}
	real, err := pathutil.Resolve(path)
	if err != nil {
		return nil, err
	}
	equal := real == filepath.Clean(path)
	if runtime.GOOS == "windows" {
		equal = strings.EqualFold(real, filepath.Clean(path))
	}
	if !equal {
		return nil, fmt.Errorf("credential path crosses a link or reparse point")
	}
	return info, nil
}
