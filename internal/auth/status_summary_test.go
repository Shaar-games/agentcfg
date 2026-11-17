package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/config"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

type summaryReadOnlyStore struct {
	records map[string][]byte
	err     error
	reads   int
	writes  int
	locks   int
}

func (s *summaryReadOnlyStore) Load(ctx context.Context, key string) ([]byte, error) {
	s.reads++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	if data, found := s.records[key]; found {
		return append([]byte(nil), data...), nil
	}
	return nil, ErrNotFound
}

func (s *summaryReadOnlyStore) Save(context.Context, string, []byte) error {
	s.writes++
	return errors.New("inspection tried to save credentials")
}

func (s *summaryReadOnlyStore) Delete(context.Context, string) error {
	s.writes++
	return errors.New("inspection tried to delete credentials")
}

func (s *summaryReadOnlyStore) Lock(context.Context, string) (func() error, error) {
	s.locks++
	return nil, errors.New("inspection tried to lock credentials")
}

type summaryHTTPClient struct{ requests int }

func (c *summaryHTTPClient) RoundTrip(*http.Request) (*http.Response, error) {
	c.requests++
	return nil, errors.New("inspection tried to contact a server")
}

func summaryServer(explicit bool) config.Server {
	server := config.Server{Name: "private-mcp", Source: "fixture", Type: "streamable-http", URL: "https://mcp.example.invalid/mcp"}
	if explicit {
		server.OAuth = &config.OAuthConfig{ClientID: "fixture-client", RedirectURL: "http://127.0.0.1:58472/oauth/callback"}
	}
	return server
}

func summaryRecord(t *testing.T, server config.Server, expired, refresh, inProgress bool) []byte {
	t.Helper()
	key := CredentialKey(InteractiveServer(server))
	expiry := time.Now().Add(time.Hour)
	if expired {
		expiry = time.Now().Add(-time.Hour)
	}
	token := &oauth2.Token{AccessToken: "DO-NOT-PRINT-ACCESS", TokenType: "Bearer", Expiry: expiry}
	if refresh {
		token.RefreshToken = "DO-NOT-PRINT-REFRESH"
	}
	record := newRecord(key, &oauth2.Config{
		ClientID: "fixture-client", ClientSecret: "DO-NOT-PRINT-CLIENT-SECRET",
		Endpoint: oauth2.Endpoint{TokenURL: "https://issuer.example.invalid/token"},
	}, token, []string{"read"}, server.URL, "https://issuer.example.invalid")
	record.RefreshInProgress = inProgress
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type summaryChangingStore struct {
	*summaryReadOnlyStore
	credentialKey   string
	credentialReads int
}

func (s *summaryChangingStore) Load(ctx context.Context, key string) ([]byte, error) {
	if key == s.credentialKey {
		s.credentialReads++
		if s.credentialReads > 1 {
			// Simulate logout between separate inspections of the same command.
			return nil, ErrNotFound
		}
	}
	return s.summaryReadOnlyStore.Load(ctx, key)
}

func TestInspectWithSummaryUsesSameCredentialSnapshot(t *testing.T) {
	server := summaryServer(true)
	key := CredentialKey(InteractiveServer(server))
	store := &summaryChangingStore{
		summaryReadOnlyStore: &summaryReadOnlyStore{records: map[string][]byte{
			key: summaryRecord(t, server, false, true, false),
		}},
		credentialKey: key,
	}
	state, summary, err := InspectWithSummary(context.Background(), server, Options{Home: t.TempDir(), Store: store})
	if err != nil || !state.Stored || !state.Authenticated || summary.State != SummaryCredentialsSaved {
		t.Fatalf("detailed status and summary do not describe the same credentials: %+v, %+v, %v", state, summary, err)
	}
	if store.credentialReads != 1 || store.writes != 0 || store.locks != 0 {
		t.Fatalf("inspection reread or modified credentials: reads=%d writes=%d locks=%d", store.credentialReads, store.writes, store.locks)
	}
}

func TestInspectSummaryUsesOnlyLocalEvidence(t *testing.T) {
	for _, test := range []struct {
		name                       string
		explicit, stored, expired  bool
		refresh, refreshInProgress bool
		want                       SummaryState
	}{
		{name: "unseen-url-only", want: SummaryUnknown},
		{name: "configured-without-grant", explicit: true, want: SummaryLoginRequired},
		{name: "configured-unexpired", explicit: true, stored: true, want: SummaryCredentialsSaved},
		{name: "url-only-unexpired", stored: true, want: SummaryCredentialsSaved},
		{name: "expired-without-refresh", explicit: true, stored: true, expired: true, want: SummaryLoginRequired},
		{name: "known-url-only-expired", stored: true, expired: true, want: SummaryLoginRequired},
		{name: "refresh-available", explicit: true, stored: true, expired: true, refresh: true, want: SummaryRefreshAvailable},
		{name: "url-only-refresh-available", stored: true, expired: true, refresh: true, want: SummaryRefreshAvailable},
		{name: "unexpired-with-refresh", stored: true, refresh: true, want: SummaryCredentialsSaved},
		{name: "interrupted-refresh", explicit: true, stored: true, refresh: true, refreshInProgress: true, want: SummaryLoginRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := summaryServer(test.explicit)
			before, _ := json.Marshal(server)
			store := &summaryReadOnlyStore{records: map[string][]byte{}}
			if test.stored {
				store.records[CredentialKey(InteractiveServer(server))] = summaryRecord(t, server, test.expired, test.refresh, test.refreshInProgress)
			}
			client := &summaryHTTPClient{}
			fetches := 0
			summary, err := InspectSummary(context.Background(), server, Options{
				Home: t.TempDir(), Store: store, HTTPClient: &http.Client{Transport: client},
				AuthorizationCodeFetcher: func(context.Context, *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
					fetches++
					return nil, errors.New("inspection tried to authorize")
				},
			})
			if err != nil || summary.State != test.want {
				t.Fatalf("state = %+v, err = %v; want %s", summary, err, test.want)
			}
			if summary.LoginCommand != "" && test.want != SummaryLoginRequired {
				t.Fatalf("inspection requested an unnecessary fresh login: %+v", summary)
			}
			if test.want == SummaryLoginRequired && summary.LoginCommand != "agentcfg auth login private-mcp" {
				t.Fatalf("missing actionable login command: %+v", summary)
			}
			after, _ := json.Marshal(server)
			if !bytes.Equal(before, after) || store.writes != 0 || store.locks != 0 || client.requests != 0 || fetches != 0 {
				t.Fatalf("inspection mutated settings/store or performed I/O: writes=%d locks=%d requests=%d fetches=%d", store.writes, store.locks, client.requests, fetches)
			}
			encoded, err := json.Marshal(summary)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"DO-NOT-PRINT", "issuer.example.invalid", "mcp.example.invalid", "58472", "fixture-client"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("summary leaked credential/provider details: %s", encoded)
				}
			}
		})
	}
}

func TestInspectSummaryObservedChallengesKeepLoginRequiredUntilSatisfied(t *testing.T) {
	for _, test := range []struct {
		name                string
		stored, replacement bool
		required            bool
		scopes              []string
		want                SummaryState
	}{
		{"first-login-challenge", false, false, true, []string{"read"}, SummaryLoginRequired},
		{"rejected-unexpired-token", true, false, true, []string{"read"}, SummaryLoginRequired},
		{"new-grant-covering-required-scopes", true, true, true, []string{"read"}, SummaryCredentialsSaved},
		{"new-grant-still-missing-required-scopes", true, true, true, []string{"extra:read"}, SummaryLoginRequired},
		{"known-protected-after-logout", false, false, false, nil, SummaryLoginRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := summaryServer(false)
			key := CredentialKey(InteractiveServer(server))
			store := &summaryReadOnlyStore{records: map[string][]byte{}}
			requirement := &authorizationRequirement{Version: 1, Identity: key, Required: test.required, Scopes: test.scopes}
			if test.stored {
				data := summaryRecord(t, server, false, true, false)
				store.records[key] = data
				var record credentialRecord
				if err := json.Unmarshal(data, &record); err != nil {
					t.Fatal(err)
				}
				requirement.Generation = record.Generation
				if test.replacement {
					requirement.Generation = "an-older-grant"
				}
			}
			data, err := json.Marshal(requirement)
			if err != nil {
				t.Fatal(err)
			}
			store.records[authorizationRequirementKey(key)] = data
			summary, err := InspectSummary(context.Background(), server, Options{Home: t.TempDir(), Store: store})
			if err != nil || summary.State != test.want {
				t.Fatalf("observed requirement was misreported: %+v, %v; want %s", summary, err, test.want)
			}
			if test.want == SummaryLoginRequired && summary.LoginCommand != "agentcfg auth login private-mcp" {
				t.Fatalf("observed requirement has no login command: %+v", summary)
			}
			if store.writes != 0 || store.locks != 0 {
				t.Fatal("inspection modified the observed requirement")
			}
		})
	}
}

func TestInspectSummaryPreservesStorageFailuresWithoutSecretText(t *testing.T) {
	const secret = "https://secret.example.invalid/?access_token=DO-NOT-PRINT"
	for _, test := range []struct {
		name string
		err  error
		want SummaryState
	}{
		{"secure-store-unavailable", fmt.Errorf("%w: %s", ErrSecureStoreUnavailable, secret), SummaryStoreUnavailable},
		{"store-corrupt", fmt.Errorf("%w: %s", ErrStoreCorrupt, secret), SummaryStoreError},
		{"permissions", errors.New(secret), SummaryStoreError},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &summaryReadOnlyStore{err: test.err}
			server := summaryServer(true)
			server.OAuth.Account = "fixture-account"
			state, summary, err := InspectWithSummary(context.Background(), server, Options{Home: t.TempDir(), Store: store})
			if err != nil || summary.State != test.want || summary.LoginCommand != "" {
				t.Fatalf("storage failure became a login/success: %+v, %v", summary, err)
			}
			if !reflect.DeepEqual(state, Status{Enabled: true, Account: "fixture-account"}) {
				t.Fatalf("storage failure exposed unverified credential status: %+v", state)
			}
			encoded, _ := json.Marshal(summary)
			if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "DO-NOT-PRINT") || store.writes != 0 || store.locks != 0 {
				t.Fatalf("unsafe storage-failure summary: %s", encoded)
			}
		})
	}
}

func TestInspectSummaryDoesNotCreateCredentialDirectories(t *testing.T) {
	root := t.TempDir()
	store, err := NewEncryptedFileStore(root, bytes.Repeat([]byte{0x34}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, explicit := range []bool{false, true} {
		if _, err := InspectSummary(context.Background(), summaryServer(explicit), Options{Home: root, Store: store}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("inspection created credentials or locks: %v, %v", entries, err)
	}
}

func TestInspectSummarySkipsUnmanagedAuthenticationAndHonorsCancellation(t *testing.T) {
	store := &summaryReadOnlyStore{err: errors.New("must not inspect this store")}
	for _, server := range []config.Server{
		{Name: "local", Type: "stdio", Command: "inert-fixture"},
		{Name: "bearer", Type: "streamable-http", URL: "https://mcp.example.invalid", Headers: map[string]string{"Authorization": "Bearer DO-NOT-PRINT"}},
	} {
		summary, err := InspectSummary(context.Background(), server, Options{Store: store})
		if err != nil || summary.State != SummaryNotManaged || summary.LoginCommand != "" || store.reads != 0 {
			t.Fatalf("unmanaged server accessed OAuth credentials: %+v, %v", summary, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InspectSummary(ctx, summaryServer(true), Options{Store: store}); !errors.Is(err, context.Canceled) || store.reads != 0 {
		t.Fatalf("cancelled inventory performed credential inspection: %v (%d reads)", err, store.reads)
	}
}

func TestLoginCommandPreservesLiteralServerNames(t *testing.T) {
	for _, test := range []struct{ name, goos, want string }{
		{"atlassian-mcp", "windows", "agentcfg auth login atlassian-mcp"},
		{"nuxt-ui", "linux", "agentcfg auth login nuxt-ui"},
		{"Nuxt UI", "windows", "agentcfg auth login 'Nuxt UI'"},
		{"Nuxt UI", "linux", "agentcfg auth login 'Nuxt UI'"},
		{"l'équipe", "windows", "agentcfg auth login 'l''équipe'"},
		{"l'équipe", "linux", "agentcfg auth login 'l'\\''équipe'"},
		{"$(touch /tmp/unexpected); echo", "linux", "agentcfg auth login '$(touch /tmp/unexpected); echo'"},
		{"$(Write-Output unexpected); echo", "windows", "agentcfg auth login '$(Write-Output unexpected); echo'"},
		{"", "windows", "agentcfg auth login ''"},
	} {
		if got := loginCommandForOS(test.name, test.goos); got != test.want {
			t.Errorf("login command for %q on %s = %q, want %q", test.name, test.goos, got, test.want)
		}
	}
}
