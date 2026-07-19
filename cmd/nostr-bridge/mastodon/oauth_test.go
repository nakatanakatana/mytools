package mastodon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

var mastodonScope = bridgestore.SourceScope{Provider: "mastodon", Account: "alice@social.example"}

func TestStartAuthorizationUsesUniqueStatePKCEAndExactScopes(t *testing.T) {
	client, store, _ := newOAuthTestClient(t, "alice", time.Now())
	one, err := client.StartAuthorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	two, err := client.StartAuthorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u1, _ := url.Parse(one)
	u2, _ := url.Parse(two)
	if u1.Path != "/oauth/authorize" || u1.Query().Get("scope") != OAuthScopes || u1.Query().Get("response_type") != "code" || u1.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize URL = %q", one)
	}
	if u1.Query().Get("state") == u2.Query().Get("state") {
		t.Fatal("states are equal")
	}
	session, err := store.OAuthSessionByState(context.Background(), mastodonScope, u1.Query().Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(session.EncryptedPayload), u1.Query().Get("code_challenge")) {
		t.Fatal("session is plaintext")
	}
	payload, err := client.openSession(session.EncryptedPayload)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(payload.CodeVerifier))
	if u1.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(hash[:]) {
		t.Fatal("challenge is not S256 verifier")
	}
}

func TestCallbackNormalizesLocalAccountAndPersistsEncryptedToken(t *testing.T) {
	client, store, forms := newOAuthTestClient(t, "alice", time.Now())
	state := startAuthorization(t, client)
	response := callback(t, client, state, "authorization-secret")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%q", response.Code, response.Body.String())
	}
	if forms.token.Get("code_verifier") == "" || forms.token.Get("redirect_uri") != "https://bridge.example/oauth/mastodon/callback" {
		t.Fatalf("token form = %#v", forms.token)
	}
	token, err := store.OAuthTokenByAccountDID(context.Background(), mastodonScope, mastodonScope.Account)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(token.EncryptedPayload), "access-secret") || strings.Contains(string(token.EncryptedPayload), "refresh-secret") {
		t.Fatal("token persisted in plaintext")
	}
	got, err := client.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "access-secret" || got.RefreshToken != "refresh-secret" {
		t.Fatalf("token = %#v", got)
	}
}

func TestCallbackRejectsDifferentAccountAndConsumesState(t *testing.T) {
	client, store, _ := newOAuthTestClient(t, "mallory", time.Now())
	state := startAuthorization(t, client)
	response := callback(t, client, state, "code")
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
	if _, err := store.OAuthTokenByAccountDID(context.Background(), mastodonScope, mastodonScope.Account); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("token err = %v", err)
	}
	if _, err := store.OAuthSessionByState(context.Background(), mastodonScope, state); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("session err = %v", err)
	}
}

func TestCallbackRejectsExpiredAndUnknownState(t *testing.T) {
	now := time.Now()
	client, store, _ := newOAuthTestClient(t, "alice", now)
	state := startAuthorization(t, client)
	client.now = func() time.Time { return now.Add(11 * time.Minute) }
	for _, value := range []string{state, "unknown"} {
		response := callback(t, client, value, "code")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("state %q status = %d", value, response.Code)
		}
	}
	if _, err := store.OAuthSessionByState(context.Background(), mastodonScope, state); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired session err = %v", err)
	}
	if _, err := store.OAuthTokenByAccountDID(context.Background(), mastodonScope, mastodonScope.Account); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("token err = %v", err)
	}
}

func TestNewOAuthClientRejectsMiswiredScope(t *testing.T) {
	for _, scope := range []bridgestore.SourceScope{
		{Provider: "bluesky", Account: mastodonScope.Account},
		{Provider: "mastodon", Account: "mallory@social.example"},
		{Provider: "mastodon", Account: "Alice@Social.Example"},
		{Provider: "mastodon", Account: "alice"},
	} {
		_, err := NewOAuthClient(OAuthOptions{Scope: scope, Store: &memoryOAuthStore{}, BaseURL: "https://social.example", Account: mastodonScope.Account, ClientID: "client", ClientSecret: "secret", RedirectURL: "https://bridge.example/oauth/mastodon/callback", EncryptionKey: make([]byte, 32)})
		if err == nil {
			t.Fatalf("accepted scope %#v", scope)
		}
	}
}

func TestCallbackPersistsOnlyWithinConfiguredScope(t *testing.T) {
	client, store, _ := newOAuthTestClient(t, "alice", time.Now())
	state := startAuthorization(t, client)
	if got := callback(t, client, state, "code"); got.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", got.Code)
	}
	for _, scope := range []bridgestore.SourceScope{
		{Provider: "bluesky", Account: mastodonScope.Account},
		{Provider: "mastodon", Account: "mallory@social.example"},
	} {
		if _, err := store.OAuthTokenByAccountDID(context.Background(), scope, mastodonScope.Account); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("scope %#v token err = %v", scope, err)
		}
	}
}

func TestExpiredTokenRefreshesWithoutLeakingSecretsInErrors(t *testing.T) {
	now := time.Now()
	client, _, forms := newOAuthTestClient(t, "alice", now)
	state := startAuthorization(t, client)
	if got := callback(t, client, state, "code"); got.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d", got.Code)
	}
	client.now = func() time.Time { return now.Add(2 * time.Hour) }
	token, err := client.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "refreshed-access" || forms.refresh.Get("refresh_token") != "refresh-secret" || forms.refresh.Get("scope") != OAuthScopes {
		t.Fatalf("token=%#v refresh=%#v", token, forms.refresh)
	}
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("transport includes refresh-secret")
	})}
	client.now = func() time.Time { return now.Add(4 * time.Hour) }
	_, err = client.Token(context.Background())
	if err == nil || strings.Contains(err.Error(), "refresh-secret") {
		t.Fatalf("error = %v", err)
	}
}

type recordedForms struct{ token, refresh url.Values }

func newOAuthTestClient(t *testing.T, acct string, now time.Time) (*OAuthClient, *memoryOAuthStore, *recordedForms) {
	t.Helper()
	forms := &recordedForms{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") == "refresh_token" {
				forms.refresh = r.Form
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "refreshed-access", "scope": OAuthScopes, "expires_in": 3600})
				return
			}
			forms.token = r.Form
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-secret", "refresh_token": "refresh-secret", "scope": OAuthScopes, "expires_in": 3600})
		case "/api/v1/accounts/verify_credentials":
			if r.Header.Get("Authorization") != "Bearer access-secret" {
				t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"acct": acct})
		default:
			t.Fatalf("path = %q", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	store := &memoryOAuthStore{sessions: map[string]bridgestore.OAuthSession{}, tokens: map[string]bridgestore.OAuthToken{}}
	target, _ := url.Parse(server.URL)
	httpClient := &http.Client{Transport: rewriteTransport{target: target, base: server.Client().Transport}}
	client, err := NewOAuthClient(OAuthOptions{Scope: mastodonScope, Store: store, HTTPClient: httpClient, BaseURL: "https://social.example", Account: mastodonScope.Account, ClientID: "client", ClientSecret: "client-secret", RedirectURL: "https://bridge.example/oauth/mastodon/callback", EncryptionKey: make([]byte, 32), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return client, store, forms
}
func startAuthorization(t *testing.T, c *OAuthClient) string {
	t.Helper()
	raw, err := c.StartAuthorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	return u.Query().Get("state")
}
func callback(t *testing.T, c *OAuthClient, state, code string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c.HandleCallback(w, httptest.NewRequest(http.MethodGet, "/oauth/mastodon/callback?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), nil))
	return w
}

type memoryOAuthStore struct {
	sessions map[string]bridgestore.OAuthSession
	tokens   map[string]bridgestore.OAuthToken
}

func (s *memoryOAuthStore) SaveOAuthSession(_ context.Context, scope bridgestore.SourceScope, v bridgestore.OAuthSession) error {
	s.sessions[scope.Provider+"\x00"+scope.Account+"\x00"+v.State] = v
	return nil
}
func (s *memoryOAuthStore) OAuthSessionByState(_ context.Context, scope bridgestore.SourceScope, state string) (bridgestore.OAuthSession, error) {
	v, ok := s.sessions[scope.Provider+"\x00"+scope.Account+"\x00"+state]
	if !ok {
		return v, sql.ErrNoRows
	}
	return v, nil
}
func (s *memoryOAuthStore) DeleteOAuthSession(_ context.Context, scope bridgestore.SourceScope, state string) error {
	delete(s.sessions, scope.Provider+"\x00"+scope.Account+"\x00"+state)
	return nil
}
func (s *memoryOAuthStore) SaveOAuthToken(_ context.Context, scope bridgestore.SourceScope, v bridgestore.OAuthToken) error {
	s.tokens[scope.Provider+"\x00"+scope.Account+"\x00"+v.AccountDID] = v
	return nil
}
func (s *memoryOAuthStore) OAuthTokenByAccountDID(_ context.Context, scope bridgestore.SourceScope, id string) (bridgestore.OAuthToken, error) {
	v, ok := s.tokens[scope.Provider+"\x00"+scope.Account+"\x00"+id]
	if !ok {
		return v, sql.ErrNoRows
	}
	return v, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type rewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	return t.base.RoundTrip(clone)
}
