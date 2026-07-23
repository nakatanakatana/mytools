package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

func TestRefreshEnumsHaveFixedValues(t *testing.T) {
	reasons := map[RefreshReason]string{
		RefreshReasonAuthorizationCode: "authorization_code",
		RefreshReasonOnDemand:          "on_demand",
		RefreshReasonMaintenance:       "maintenance",
	}
	for reason, want := range reasons {
		if got := string(reason); got != want {
			t.Errorf("refresh reason = %q, want %q", got, want)
		}
	}
	classes := map[RefreshErrorClass]string{
		RefreshErrorTimeout:             "timeout",
		RefreshErrorConnection:          "connection",
		RefreshErrorRateLimit:           "rate_limited",
		RefreshErrorServer:              "server",
		RefreshErrorInvalidGrant:        "invalid_grant",
		RefreshErrorMissingRefreshToken: "missing_refresh_token",
		RefreshErrorDecrypt:             "decrypt",
		RefreshErrorDPoPKey:             "dpop_key",
		RefreshErrorProtocol:            "protocol",
	}
	for class, want := range classes {
		if got := string(class); got != want {
			t.Errorf("refresh error class = %q, want %q", got, want)
		}
	}
}

func TestAuthorizationStatusUsesLegacyTimestampForExpiredRefreshableToken(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	client, store := newStatusTestClient(t, now)
	stored := saveStatusTestToken(t, client, store, tokenPayload{
		AccessToken:  "expired-access",
		RefreshToken: "refresh-token",
		DPoPKey:      newStatusTestJWK(t),
		Expiry:       now.Add(-time.Minute),
	}, bridgestore.OAuthToken{AccountDID: "did:plc:alice", UpdatedAt: now.Add(-7 * 24 * time.Hour).Unix()})
	client.store = failPersistenceOAuthStore{OAuthStore: store, t: t}

	status, err := client.AuthorizationStatus(context.Background(), stored.AccountDID, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if status.AccessTokenValid || !status.AuthorizationAvailable || status.ReauthRequired {
		t.Fatalf("status = %#v", status)
	}
	if got, want := status.LastRefreshSucceededAt, time.Unix(stored.UpdatedAt, 0); !got.Equal(want) {
		t.Fatalf("last refresh = %v, want %v", got, want)
	}
	if got, want := status.NextMaintenanceRefresh, time.Unix(stored.UpdatedAt, 0).Add(30*24*time.Hour); !got.Equal(want) {
		t.Fatalf("next refresh = %v, want %v", got, want)
	}
}

type failPersistenceOAuthStore struct {
	bridgestore.OAuthStore
	t *testing.T
}

func (s failPersistenceOAuthStore) SaveOAuthSession(context.Context, bridgestore.SourceScope, bridgestore.OAuthSession) error {
	s.t.Fatal("AuthorizationStatus saved an OAuth session")
	return errors.New("unexpected persistence")
}

func (s failPersistenceOAuthStore) DeleteOAuthSession(context.Context, bridgestore.SourceScope, string) error {
	s.t.Fatal("AuthorizationStatus deleted an OAuth session")
	return errors.New("unexpected persistence")
}

func (s failPersistenceOAuthStore) SaveOAuthToken(context.Context, bridgestore.SourceScope, bridgestore.OAuthToken) error {
	s.t.Fatal("AuthorizationStatus saved an OAuth token")
	return errors.New("unexpected persistence")
}

func (s failPersistenceOAuthStore) UpdateOAuthTokenRefreshFailure(context.Context, bridgestore.SourceScope, string, string, bool) error {
	s.t.Fatal("AuthorizationStatus updated an OAuth refresh failure")
	return errors.New("unexpected persistence")
}

func TestAuthorizationStatusReportsMissingRefreshToken(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	client, store := newStatusTestClient(t, now)
	stored := saveStatusTestToken(t, client, store, tokenPayload{
		AccessToken: "access-token",
		DPoPKey:     newStatusTestJWK(t),
		Expiry:      now.Add(time.Hour),
	}, bridgestore.OAuthToken{AccountDID: "did:plc:alice", LastRefreshAt: now.Unix()})

	status, err := client.AuthorizationStatus(context.Background(), stored.AccountDID, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !status.AccessTokenValid || status.AuthorizationAvailable || !status.ReauthRequired || status.LastRefreshErrorClass != RefreshErrorMissingRefreshToken {
		t.Fatalf("status = %#v", status)
	}
}

func TestAuthorizationStatusReportsDecryptFailure(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	client, store := newStatusTestClient(t, now)
	stored := bridgestore.OAuthToken{AccountDID: "did:plc:alice", EncryptedPayload: []byte("not encrypted"), LastRefreshAt: now.Unix()}
	if err := store.SaveOAuthToken(context.Background(), oauthTestScope, stored); err != nil {
		t.Fatal(err)
	}

	status, err := client.AuthorizationStatus(context.Background(), stored.AccountDID, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if status.AccessTokenValid || status.AuthorizationAvailable || !status.ReauthRequired || status.LastRefreshErrorClass != RefreshErrorDecrypt {
		t.Fatalf("status = %#v", status)
	}
}

func TestAuthorizationStatusReportsInvalidDPoPKey(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	client, store := newStatusTestClient(t, now)
	stored := saveStatusTestToken(t, client, store, tokenPayload{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		DPoPKey:      jwk{Kty: "EC", Crv: "P-256", X: "invalid", Y: "invalid", D: "invalid"},
		Expiry:       now.Add(time.Hour),
	}, bridgestore.OAuthToken{AccountDID: "did:plc:alice", LastRefreshAt: now.Unix()})

	status, err := client.AuthorizationStatus(context.Background(), stored.AccountDID, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if status.AccessTokenValid || status.AuthorizationAvailable || !status.ReauthRequired || status.LastRefreshErrorClass != RefreshErrorDPoPKey {
		t.Fatalf("status = %#v", status)
	}
}

func TestAuthorizationStatusReportsPersistedReauthorizationState(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	client, store := newStatusTestClient(t, now)
	stored := saveStatusTestToken(t, client, store, tokenPayload{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		DPoPKey:      newStatusTestJWK(t),
		Expiry:       now.Add(time.Hour),
	}, bridgestore.OAuthToken{
		AccountDID:            "did:plc:alice",
		LastRefreshAt:         now.Unix(),
		ReauthRequired:        true,
		LastRefreshErrorClass: string(RefreshErrorInvalidGrant),
	})

	status, err := client.AuthorizationStatus(context.Background(), stored.AccountDID, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !status.AccessTokenValid || status.AuthorizationAvailable || !status.ReauthRequired || status.LastRefreshErrorClass != RefreshErrorInvalidGrant {
		t.Fatalf("status = %#v", status)
	}
}

func newStatusTestClient(t *testing.T, now time.Time) (*Client, bridgestore.OAuthStore) {
	t.Helper()
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("AuthorizationStatus made an HTTP request")
		return nil, errors.New("unexpected HTTP request")
	})}
	client, store := newTestClient(t, "https://issuer.example", httpClient)
	client.now = func() time.Time { return now }
	return client, store
}

func newStatusTestJWK(t *testing.T) jwk {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return privateJWK(key)
}

func saveStatusTestToken(t *testing.T, client *Client, store bridgestore.OAuthStore, payload tokenPayload, stored bridgestore.OAuthToken) bridgestore.OAuthToken {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	stored.EncryptedPayload, err = client.encrypt(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveOAuthToken(context.Background(), oauthTestScope, stored); err != nil {
		t.Fatal(err)
	}
	return stored
}
