package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// An observed challenge is diagnostic state, separate from credentials. Keep it
// in the same encrypted Store and bind it to the exact rejected grant. A later
// successful login or refresh changes Generation and supersedes this hint once
// the grant includes every permission that the resource actually required.
type authorizationRequirement struct {
	Version    int      `json:"version"`
	Identity   string   `json:"identity"`
	Generation string   `json:"generation,omitempty"`
	Required   bool     `json:"required"`
	Scopes     []string `json:"scopes,omitempty"`
}

func authorizationRequirementKey(key string) string {
	return "agentcfg.oauth.requirement.v1:" + key
}

func loadAuthorizationRequirement(ctx context.Context, store Store, key string) (*authorizationRequirement, error) {
	data, err := store.Load(ctx, authorizationRequirementKey(key))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer clear(data)
	var requirement authorizationRequirement
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&requirement) != nil || decoder.Decode(new(any)) != io.EOF ||
		requirement.Version != 1 || requirement.Identity != key {
		return nil, ErrStoreCorrupt
	}
	if !validAuthorizationScopes(requirement.Scopes) {
		return nil, ErrStoreCorrupt
	}
	return &requirement, nil
}

func validAuthorizationScopes(scopes []string) bool {
	for _, scope := range scopes {
		if scope == "" || strings.IndexFunc(scope, func(c rune) bool {
			return c < 0x21 || c > 0x7e || c == '"' || c == '\\'
		}) >= 0 {
			return false
		}
	}
	return true
}

func saveAuthorizationRequirement(ctx context.Context, store Store, requirement *authorizationRequirement) error {
	data, err := json.Marshal(requirement)
	if err != nil {
		return safeError("encode OAuth login requirement", err)
	}
	defer clear(data)
	if err := store.Save(ctx, authorizationRequirementKey(requirement.Identity), data); err != nil {
		return safeError("save OAuth login requirement", err)
	}
	return nil
}

func requirementApplies(requirement *authorizationRequirement, record *credentialRecord) bool {
	return requirement != nil && requirement.Required &&
		(record == nil || requirement.Generation == record.Generation || !includesScopes(record.Scopes, requirement.Scopes))
}

// Observe only a real resource challenge. A fresh token saved by another
// process must not be declared invalid because an older request lacked it.
// Existing unresolved scopes for the same grant accumulate across requests.
func noteAuthorizationChallenge(ctx context.Context, store Store, locker Locker, key, authorization string, scopes []string) error {
	if !validAuthorizationScopes(scopes) {
		return fmt.Errorf("OAuth challenge contains invalid scope tokens")
	}
	unlock, err := locker.Lock(ctx, key)
	if err != nil {
		return err
	}
	defer unlock()
	record, err := loadRecord(ctx, store, key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	previous, err := loadAuthorizationRequirement(ctx, store, key)
	if err != nil {
		return err
	}
	requirement := &authorizationRequirement{Version: 1, Identity: key, Scopes: unionScopes(scopes)}
	if record == nil {
		requirement.Required = true
	} else {
		requirement.Generation = record.Generation
		requirement.Required = record.RefreshInProgress ||
			(!record.Token.Valid() && record.Token.RefreshToken == "") ||
			(authorization != "" && authorization == "Bearer "+record.Token.AccessToken) ||
			!includesScopes(record.Scopes, scopes)
	}
	if requirementApplies(previous, record) {
		requirement.Required = true
		requirement.Scopes = unionScopes(previous.Scopes, requirement.Scopes)
	}
	return saveAuthorizationRequirement(ctx, store, requirement)
}
