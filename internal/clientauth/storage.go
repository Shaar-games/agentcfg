package clientauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"agentcfg/internal/auth"
)

var errCapacity = errors.New("authorization capacity reached")

type client struct {
	ID           string   `json:"client_id"`
	Name         string   `json:"client_name"`
	Redirects    []string `json:"redirect_uris"`
	Application  string   `json:"application_type"`
	GrantTypes   []string `json:"grant_types"`
	RegisteredAt int64    `json:"client_id_issued_at"`
	LastUsed     int64    `json:"last_used,omitempty"`
}

type clientState struct {
	Version int               `json:"version"`
	Clients map[string]client `json:"clients"`
}

type grant struct {
	ID         string `json:"id"`
	ClientID   string `json:"client_id"`
	Resource   string `json:"resource"`
	Identity   string `json:"identity"`
	Scope      string `json:"scope"`
	ExpiresAt  int64  `json:"expires_at"`
	Revoked    bool   `json:"revoked,omitempty"`
	RevokedAt  int64  `json:"revoked_at,omitempty"`
	Pending    bool   `json:"pending,omitempty"`
	AccessKey  []byte `json:"access_key"`
	RefreshKey []byte `json:"refresh_key"`
	Generation uint64 `json:"generation"`
	Refresh    bool   `json:"refresh"`
}

type authorizationCode struct {
	GrantID   string `json:"grant_id"`
	Redirect  string `json:"redirect_uri"`
	Challenge string `json:"challenge"`
	ExpiresAt int64  `json:"expires_at"`
}

type grantState struct {
	Version int                          `json:"version"`
	Grants  map[string]grant             `json:"grants"`
	Codes   map[string]authorizationCode `json:"codes"`
}

func (a *Authority) clients(ctx context.Context, update func(*clientState) (bool, error)) (err error) {
	// One transaction lock covers both namespaces: registration cleanup must
	// observe grant activation/revocation consistently across authority instances.
	unlock, err := a.locker.Lock(ctx, a.lockKey)
	if err != nil {
		return storeFailure(ctx)
	}
	defer func() {
		if unlock() != nil && err == nil {
			err = ErrUnavailable
		}
	}()
	state := clientState{Version: 1, Clients: make(map[string]client)}
	if err := a.load(ctx, a.clientKey, &state); err != nil {
		return err
	}
	if state.Version != 1 || state.Clients == nil || len(state.Clients) > maxClients {
		return ErrUnavailable
	}
	grants, _, err := a.readGrants(ctx)
	if err != nil {
		return err
	}
	now := a.now().Unix()
	active := make(map[string]bool)
	for _, record := range grants.Grants {
		// A short grace period preserves cached client IDs while a legitimate
		// client responds to an upstream challenge with fresh authorization.
		if !record.Pending && (!record.Revoked || record.RevokedAt > now-int64(clientIdleTTL.Seconds())) {
			active[record.ClientID] = true
		}
	}
	pruned := false
	for id, record := range state.Clients {
		if max(record.RegisteredAt, record.LastUsed) <= now-int64(clientIdleTTL.Seconds()) && !active[id] {
			delete(state.Clients, id)
			pruned = true
		}
	}
	changed, operationErr := update(&state)
	if changed || pruned {
		if err := a.save(ctx, a.clientKey, &state); err != nil {
			return err
		}
	}
	return operationErr
}

func (a *Authority) grants(ctx context.Context, update func(*grantState) (bool, error)) (err error) {
	unlock, err := a.locker.Lock(ctx, a.lockKey)
	if err != nil {
		return storeFailure(ctx)
	}
	defer func() {
		if unlock() != nil && err == nil {
			err = ErrUnavailable
		}
	}()
	state, pruned, err := a.readGrants(ctx)
	if err != nil {
		return err
	}
	changed, operationErr := update(&state)
	if changed || pruned {
		if err := a.save(ctx, a.grantKey, &state); err != nil {
			return err
		}
	}
	return operationErr
}

// readGrants is called only while the shared transaction lock is held.
func (a *Authority) readGrants(ctx context.Context) (grantState, bool, error) {
	state := grantState{Version: 1, Grants: make(map[string]grant), Codes: make(map[string]authorizationCode)}
	if err := a.load(ctx, a.grantKey, &state); err != nil {
		return grantState{}, false, err
	}
	if state.Version != 1 || state.Grants == nil || state.Codes == nil || len(state.Grants) > maxGrants || len(state.Codes) > maxCodes {
		return grantState{}, false, ErrUnavailable
	}
	now := a.now().Unix()
	pruned := false
	for id, record := range state.Grants {
		if record.ExpiresAt <= now {
			clear(record.AccessKey)
			clear(record.RefreshKey)
			delete(state.Grants, id)
			pruned = true
		}
	}
	for code, record := range state.Codes {
		_, exists := state.Grants[record.GrantID]
		if record.ExpiresAt <= now || !exists {
			delete(state.Codes, code)
			pruned = true
		}
	}
	return state, pruned, nil
}

func (a *Authority) load(ctx context.Context, key string, target any) error {
	raw, err := a.store.Load(ctx, key)
	if errors.Is(err, auth.ErrNotFound) {
		return nil
	}
	if err != nil {
		return storeFailure(ctx)
	}
	defer clear(raw)
	if len(raw) > 1<<20 || json.Unmarshal(raw, target) != nil {
		return ErrUnavailable
	}
	return nil
}

func (a *Authority) save(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 1<<20 {
		return ErrUnavailable
	}
	defer clear(raw)
	if a.store.Save(ctx, key, raw) != nil {
		return storeFailure(ctx)
	}
	return nil
}

func storeFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnavailable
}

func (a *Authority) registered(ctx context.Context, id string) (client, error) {
	var found client
	err := a.clients(ctx, func(state *clientState) (bool, error) {
		value, ok := state.Clients[id]
		if !ok || value.ID != id {
			return false, ErrInvalidToken
		}
		found = value
		return false, nil
	})
	return found, err
}

// Keep a registration alive only once the caller has supplied a valid resource,
// redirect, and PKCE request. Public DCR/invalid authorize traffic cannot renew it.
func (a *Authority) touchClient(ctx context.Context, id string) (client, error) {
	var found client
	err := a.clients(ctx, func(state *clientState) (bool, error) {
		record, ok := state.Clients[id]
		if !ok || record.ID != id {
			return false, ErrInvalidToken
		}
		record.LastUsed = a.now().Unix()
		state.Clients[id] = record
		found = record
		return true, nil
	})
	return found, err
}

func (a *Authority) register(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	var request struct {
		Name          string   `json:"client_name"`
		Redirects     []string `json:"redirect_uris"`
		Application   string   `json:"application_type"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
		AuthMethod    string   `json:"token_endpoint_auth_method"`
		Scope         string   `json:"scope"`
	}
	if decodeJSON(w, r, &request) != nil || len(request.Name) > 160 ||
		len(request.Redirects) == 0 || len(request.Redirects) > 4 ||
		(request.AuthMethod != "" && request.AuthMethod != "none") ||
		(request.Scope != "" && request.Scope != scopeAccess) ||
		(request.Application != "" && request.Application != "native" && request.Application != "web") {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata")
		return
	}
	native := false
	for i, raw := range request.Redirects {
		u, ok := validRedirect(raw)
		if !ok || slices.Contains(request.Redirects[:i], raw) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri")
			return
		}
		native = native || u.Scheme == "http"
	}
	if len(request.GrantTypes) == 0 {
		request.GrantTypes = []string{"authorization_code"}
	}
	if len(request.GrantTypes) > 2 || !slices.Contains(request.GrantTypes, "authorization_code") {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata")
		return
	}
	for i, value := range request.GrantTypes {
		if (value != "authorization_code" && value != "refresh_token") || slices.Contains(request.GrantTypes[:i], value) {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata")
			return
		}
	}
	if len(request.ResponseTypes) > 1 || (len(request.ResponseTypes) == 1 && request.ResponseTypes[0] != "code") {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata")
		return
	}
	if request.Application == "" {
		request.Application = "web"
		if native {
			request.Application = "native"
		}
	}
	id, err := opaque(32)
	if err != nil {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	c := client{
		ID: "c1_" + id, Name: request.Name, Redirects: request.Redirects,
		Application: request.Application, GrantTypes: request.GrantTypes, RegisteredAt: a.now().Unix(), LastUsed: a.now().Unix(),
	}
	err = a.clients(r.Context(), func(state *clientState) (bool, error) {
		if len(state.Clients) >= maxClients {
			return false, errCapacity
		}
		state.Clients[c.ID] = c
		return true, nil
	})
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, errCapacity) {
			status = http.StatusTooManyRequests
		}
		oauthError(w, status, "temporarily_unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id": c.ID, "client_id_issued_at": c.RegisteredAt,
		"client_name": c.Name, "redirect_uris": c.Redirects, "application_type": c.Application,
		"grant_types": c.GrantTypes, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none", "scope": scopeAccess,
	})
}

func validRedirect(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > maxURL || u.Host == "" || u.User != nil ||
		u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" ||
		(u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname()))) {
		return nil, false
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, false
	}
	for _, reserved := range []string{"code", "state", "iss", "error", "error_description"} {
		if q.Has(reserved) {
			return nil, false
		}
	}
	return u, true
}

func redirectMatches(c client, requested string) bool {
	u, valid := validRedirect(requested)
	if !valid {
		return false
	}
	for _, raw := range c.Redirects {
		if raw == requested {
			return true
		}
		registered, valid := validRedirect(raw)
		if !valid || c.Application != "native" || registered.Scheme != "http" || u.Scheme != "http" {
			continue
		}
		// RFC 8252 permits only the port to differ for native loopback URIs.
		// Preserve the original spelling of every other URI component.
		copyRegistered, copyRequested := *registered, *u
		copyRegistered.Host = registered.Hostname()
		copyRequested.Host = u.Hostname()
		if copyRegistered.String() == copyRequested.String() {
			return true
		}
	}
	return false
}

// Revoke invalidates the complete local grant, including its refresh family.
// It is used when the upstream requires fresh consent; it never revokes a
// different client's grant even if a caller supplies an unrelated grant ID.
func (a *Authority) Revoke(ctx context.Context, principal Principal) error {
	if a.ctx.Err() != nil {
		return ErrUnavailable
	}
	return a.grants(ctx, func(state *grantState) (bool, error) {
		record, ok := state.Grants[principal.GrantID]
		if !ok || record.ClientID != principal.ClientID || record.Resource != principal.Resource {
			return false, ErrInvalidToken
		}
		record.Revoked = true
		record.RevokedAt = a.now().Unix()
		state.Grants[record.ID] = record
		return true, nil
	})
}

func expiration(now time.Time, duration time.Duration) int64 {
	return now.Add(duration).Unix()
}
