package clientauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope"`
}

func (a *Authority) issueCode(ctx context.Context, f *flow) (string, error) {
	code, err := opaque(32)
	if err != nil {
		return "", ErrUnavailable
	}
	id, err := opaque(32)
	if err != nil {
		return "", ErrUnavailable
	}
	record := grant{
		ID: id, ClientID: f.client.ID, Resource: f.resource.URL, Identity: f.resource.Identity,
		Scope: scopeAccess, ExpiresAt: expiration(a.now(), codeTTL), Pending: true,
		AccessKey: make([]byte, 32), RefreshKey: make([]byte, 32),
		Refresh: slices.Contains(f.client.GrantTypes, "refresh_token"),
	}
	if _, err := rand.Read(record.AccessKey); err != nil {
		return "", ErrUnavailable
	}
	if _, err := rand.Read(record.RefreshKey); err != nil {
		return "", ErrUnavailable
	}
	code = "ac1_" + code
	err = a.grants(ctx, func(state *grantState) (bool, error) {
		if len(state.Grants) >= maxGrants || len(state.Codes) >= maxCodes {
			return false, errCapacity
		}
		state.Grants[record.ID] = record
		state.Codes[fingerprint(code)] = authorizationCode{
			GrantID: record.ID, Redirect: f.redirect, Challenge: f.challenge, ExpiresAt: record.ExpiresAt,
		}
		return true, nil
	})
	return code, err
}

func (a *Authority) token(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	values, err := form(w, r)
	if err != nil || r.Header.Get("Authorization") != "" || values.Get("client_secret") != "" ||
		values.Get("client_id") == "" || len(values.Get("client_id")) > 128 {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if values.Get("scope") != "" && values.Get("scope") != scopeAccess {
		oauthError(w, http.StatusBadRequest, "invalid_scope")
		return
	}
	var response tokenResponse
	switch values.Get("grant_type") {
	case "authorization_code":
		response, err = a.exchangeCode(r.Context(), values)
	case "refresh_token":
		response, err = a.exchangeRefresh(r.Context(), values)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	if err != nil {
		a.requestFailure(w, err, "invalid_grant")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *Authority) exchangeCode(ctx context.Context, values url.Values) (tokenResponse, error) {
	var response tokenResponse
	if !validVerifier(values.Get("code_verifier")) || len(values.Get("code")) > 128 {
		return response, ErrInvalidToken
	}
	sum := sha256.Sum256([]byte(values.Get("code_verifier")))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	err := a.grants(ctx, func(state *grantState) (bool, error) {
		key := fingerprint(values.Get("code"))
		code, ok := state.Codes[key]
		if !ok {
			return false, ErrInvalidToken
		}
		record, ok := state.Grants[code.GrantID]
		if !ok || record.Revoked || !record.Pending || record.ClientID != values.Get("client_id") ||
			(values.Get("resource") != "" && record.Resource != values.Get("resource")) ||
			code.Redirect != values.Get("redirect_uri") ||
			subtle.ConstantTimeCompare([]byte(code.Challenge), []byte(challenge)) != 1 {
			return false, ErrInvalidToken
		}
		resource, err := a.resource(ctx, record.Resource)
		if err != nil || resource.Identity != record.Identity {
			return false, ErrInvalidToken
		}
		if err := validGrant(record); err != nil {
			return false, err
		}
		record.Pending = false
		record.ExpiresAt = expiration(a.now(), grantTTL)
		response, err = a.tokens(record)
		if err != nil {
			return false, err
		}
		// Code removal and grant persistence are one locked, atomic store write.
		delete(state.Codes, key)
		state.Grants[record.ID] = record
		return true, nil
	})
	return response, err
}

func (a *Authority) exchangeRefresh(ctx context.Context, values url.Values) (tokenResponse, error) {
	var response tokenResponse
	payload, signature, err := parseToken(values.Get("refresh_token"), "r1_", 40)
	if err != nil {
		return response, err
	}
	id := base64.RawURLEncoding.EncodeToString(payload[:32])
	generation := binary.BigEndian.Uint64(payload[32:40])
	err = a.grants(ctx, func(state *grantState) (bool, error) {
		record, ok := state.Grants[id]
		if !ok || record.Revoked || record.Pending || !record.Refresh || record.ClientID != values.Get("client_id") ||
			(values.Get("resource") != "" && record.Resource != values.Get("resource")) {
			return false, ErrInvalidToken
		}
		if err := validGrant(record); err != nil {
			return false, err
		}
		if !hmac.Equal(signature, a.tokenMAC(record.RefreshKey, "refresh", payload)) {
			return false, ErrInvalidToken
		}
		if generation < record.Generation {
			// Only an authentic prior refresh token can revoke the family.
			// Its MAC permits replay detection without an unbounded token log.
			record.Revoked = true
			record.RevokedAt = a.now().Unix()
			state.Grants[id] = record
			return true, ErrInvalidToken
		}
		if generation != record.Generation {
			return false, ErrInvalidToken
		}
		resource, err := a.resource(ctx, record.Resource)
		if err != nil || resource.Identity != record.Identity {
			return false, ErrInvalidToken
		}
		if record.Generation == math.MaxUint64 {
			record.Revoked = true
			record.RevokedAt = a.now().Unix()
			state.Grants[id] = record
			return true, ErrInvalidToken
		}
		record.Generation++
		response, err = a.tokens(record)
		if err != nil {
			return false, err
		}
		state.Grants[id] = record
		return true, nil
	})
	return response, err
}

func validGrant(record grant) error {
	if len(record.AccessKey) != 32 || len(record.RefreshKey) != 32 || record.Scope != scopeAccess {
		return ErrUnavailable
	}
	return nil
}

// The token format is opaque to clients. Each grant has distinct signing
// credentials stored only inside the encrypted Store. A monotonic authenticated
// refresh generation detects replay in constant storage; access tokens retain
// their own short expiry until the whole grant is revoked.
func (a *Authority) tokens(record grant) (tokenResponse, error) {
	id, err := base64.RawURLEncoding.DecodeString(record.ID)
	if err != nil || len(id) != 32 {
		return tokenResponse{}, ErrUnavailable
	}
	expires := min(expiration(a.now(), accessTTL), record.ExpiresAt)
	if expires <= a.now().Unix() {
		return tokenResponse{}, ErrInvalidToken
	}
	access := make([]byte, 56)
	copy(access, id)
	binary.BigEndian.PutUint64(access[32:40], uint64(expires))
	if _, err := rand.Read(access[40:56]); err != nil {
		return tokenResponse{}, ErrUnavailable
	}
	response := tokenResponse{
		AccessToken: "a1_" + base64.RawURLEncoding.EncodeToString(append(access, a.tokenMAC(record.AccessKey, "access", access)...)),
		TokenType:   "Bearer", ExpiresIn: expires - a.now().Unix(), Scope: record.Scope,
	}
	if record.Refresh {
		refresh := make([]byte, 40)
		copy(refresh, id)
		binary.BigEndian.PutUint64(refresh[32:40], record.Generation)
		response.RefreshToken = "r1_" + base64.RawURLEncoding.EncodeToString(append(refresh, a.tokenMAC(record.RefreshKey, "refresh", refresh)...))
	}
	return response, nil
}

func (a *Authority) tokenMAC(key []byte, purpose string, payload []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(a.issuer.String()))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(purpose))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

func parseToken(raw, prefix string, payloadSize int) ([]byte, []byte, error) {
	if !strings.HasPrefix(raw, prefix) || len(raw) > 256 {
		return nil, nil, ErrInvalidToken
	}
	encoded := strings.TrimPrefix(raw, prefix)
	value, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(value) != payloadSize+sha256.Size || base64.RawURLEncoding.EncodeToString(value) != encoded {
		return nil, nil, ErrInvalidToken
	}
	return value[:payloadSize], value[payloadSize:], nil
}

// Verify reads current persisted grant state, so revocation, rotation replay,
// and effective upstream identity changes take effect across process restarts.
func (a *Authority) Verify(ctx context.Context, bearer string, resource Resource) (Principal, error) {
	if a.ctx.Err() != nil {
		return Principal{}, ErrUnavailable
	}
	if !a.validResource(resource) {
		return Principal{}, ErrInvalidToken
	}
	payload, signature, err := parseToken(bearer, "a1_", 56)
	if err != nil {
		return Principal{}, err
	}
	id := base64.RawURLEncoding.EncodeToString(payload[:32])
	expires := binary.BigEndian.Uint64(payload[32:40])
	var principal Principal
	err = a.grants(ctx, func(state *grantState) (bool, error) {
		record, ok := state.Grants[id]
		if !ok || record.Revoked || record.Pending || record.Resource != resource.URL || record.Identity != resource.Identity ||
			expires > math.MaxInt64 || int64(expires) <= a.now().Unix() || int64(expires) > record.ExpiresAt {
			return false, ErrInvalidToken
		}
		if err := validGrant(record); err != nil {
			return false, err
		}
		if !hmac.Equal(signature, a.tokenMAC(record.AccessKey, "access", payload)) {
			return false, ErrInvalidToken
		}
		principal = Principal{ClientID: record.ClientID, GrantID: record.ID, Resource: record.Resource}
		return false, nil
	})
	if err != nil {
		return Principal{}, err
	}
	return principal, nil
}
