package auth

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestGatewayKeyReadOnlyPersistenceAndConcurrentCreation(t *testing.T) {
	root := t.TempDir()
	store, err := NewEncryptedFileStore(root, bytes.Repeat([]byte{62}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GatewayKey(t.Context(), root, store, false); !errors.Is(err, ErrGatewayNotInitialized) {
		t.Fatalf("missing key: %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("read-only key lookup created state: %v %v", entries, err)
	}
	type result struct {
		key []byte
		err error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := GatewayKey(t.Context(), root, store, true)
			results <- result{key, err}
		}()
	}
	wg.Wait()
	close(results)
	var expected []byte
	for result := range results {
		if result.err != nil || len(result.key) != 32 {
			t.Fatalf("create key: %v", result.err)
		}
		if expected == nil {
			expected = result.key
		} else if !bytes.Equal(expected, result.key) {
			t.Fatal("concurrent creators issued different routing capabilities")
		}
	}
	restarted, err := NewEncryptedFileStore(root, bytes.Repeat([]byte{62}, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := GatewayKey(t.Context(), root, restarted, false)
	if err != nil || !bytes.Equal(expected, key) {
		t.Fatal("restart changed gateway resource URLs")
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, expected) || bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString(expected))) {
			t.Errorf("routing key persisted in plaintext: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayKeyCorruptionIsNeverOverwritten(t *testing.T) {
	root := t.TempDir()
	store, err := NewEncryptedFileStore(root, bytes.Repeat([]byte{63}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(t.Context(), gatewayKeyRecord, []byte(`{"version":1,"key":"bad"}`)); err != nil {
		t.Fatal(err)
	}
	before, err := store.Load(t.Context(), gatewayKeyRecord)
	if err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		if _, err := GatewayKey(t.Context(), root, store, create); !errors.Is(err, ErrStoreCorrupt) {
			t.Fatalf("corrupt key was accepted or replaced: %v", err)
		}
	}
	after, err := store.Load(t.Context(), gatewayKeyRecord)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("unreadable routing state was replaced")
	}
}
