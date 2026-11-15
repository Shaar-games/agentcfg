package auth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrGatewayNotInitialized never causes configuration planning to create a key.
var ErrGatewayNotInitialized = errors.New("native OAuth gateway is not initialized; run agentcfg gateway start before synchronizing HTTP URLs")

const gatewayKeyRecord = "agentcfg-native-gateway-routing-key-v1"

// GatewayKey provides a persistent secret for per-resource endpoint capabilities.
// It uses the same encrypted, owner-scoped store as upstream OAuth credentials.
// Read-only callers never lock, create files, or replace an unreadable key.
func GatewayKey(ctx context.Context, home string, supplied Store, create bool) ([]byte, error) {
	store, locker, err := resolveStore(Options{Home: home, Store: supplied})
	if err != nil {
		return nil, err
	}
	if create {
		unlock, err := locker.Lock(ctx, gatewayKeyRecord)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	data, err := store.Load(ctx, gatewayKeyRecord)
	if err == nil {
		var record struct {
			Version int    `json:"version"`
			Key     []byte `json:"key"`
		}
		if len(data) > 512 || json.Unmarshal(data, &record) != nil || record.Version != 1 || len(record.Key) != 32 {
			return nil, fmt.Errorf("invalid gateway routing key: %w", ErrStoreCorrupt)
		}
		return record.Key, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if !create {
		return nil, ErrGatewayNotInitialized
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	data, err = json.Marshal(struct {
		Version int    `json:"version"`
		Key     []byte `json:"key"`
	}{1, key})
	if err != nil {
		return nil, err
	}
	if err := store.Save(ctx, gatewayKeyRecord, data); err != nil {
		clear(key)
		return nil, err
	}
	return key, nil
}
