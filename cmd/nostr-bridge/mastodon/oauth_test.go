package mastodon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
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

func TestStartAuthorizationLogsPersistenceFailure(t *testing.T) {
	client, store, _ := newOAuthTestClient(t, "alice", time.Now())
	store.saveSessionErr = errors.New("database unavailable")
	logs := captureOAuthLogs(t)

	_, err := client.StartAuthorization(context.Background())
	if err == nil {
		t.Fatal("StartAuthorization error = nil")
	}
	assertOAuthLogContains(t, logs.String(), "stage=authorization_start", "result=started", "stage=session_persistence", "result=failed", "database unavailable")
	assertOAuthLogExcludes(t, logs.String(), "client-secret")
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
	logs := captureOAuthLogs(t)
	token, err := client.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "refreshed-access" || forms.refresh.Get("refresh_token") != "refresh-secret" || forms.refresh.Get("scope") != OAuthScopes {
		t.Fatalf("token=%#v refresh=%#v", token, forms.refresh)
	}
	assertOAuthLogContains(t, logs.String(), "stage=token_refresh", "result=started", "result=succeeded")
	assertOAuthLogExcludes(t, logs.String(), "access-secret", "refresh-secret", "refreshed-access", "client-secret")
	logs.Reset()
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("transport includes refresh-secret")
	})}
	client.now = func() time.Time { return now.Add(4 * time.Hour) }
	_, err = client.Token(context.Background())
	if err == nil || strings.Contains(err.Error(), "refresh-secret") {
		t.Fatalf("error = %v", err)
	}
	assertOAuthLogContains(t, logs.String(), "stage=token_refresh", "result=started", "result=failed")
	assertOAuthLogExcludes(t, logs.String(), "access-secret", "refresh-secret", "refreshed-access", "client-secret")
}

func TestOAuthRemoteErrorsRetainSafeDiagnosticsAndRedactRequestSecrets(t *testing.T) {
	client, _, _ := newOAuthTestClient(t, "alice", time.Now())
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"error":"invalid_grant\n","error_description":"code authorization-secret rejected ` + strings.Repeat("x", 400) + `"}`
		return &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	_, err := client.exchange(context.Background(), url.Values{
		"grant_type": {"authorization_code"}, "code": {"authorization-secret"}, "client_secret": {"client-secret"},
	})
	if err == nil {
		t.Fatal("exchange error = nil")
	}
	text := err.Error()
	for _, want := range []string{"token exchange", "status=400", `error="invalid_grant"`, "description="} {
		if !strings.Contains(text, want) {
			t.Errorf("error %q missing %q", text, want)
		}
	}
	for _, secret := range []string{"authorization-secret", "client-secret", "\n"} {
		if strings.Contains(text, secret) {
			t.Errorf("error contains unsafe value %q: %q", secret, text)
		}
	}
	if len(text) > 512 {
		t.Fatalf("error is unbounded: %d bytes", len(text))
	}
}

func TestOAuthTransportErrorsExposeOnlySafeClassifications(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "context canceled", err: context.Canceled, want: "context_canceled"},
		{name: "deadline exceeded", err: context.DeadlineExceeded, want: "deadline_exceeded"},
		{name: "DNS", err: &net.DNSError{Err: "lookup authorization-secret", Name: "secret.example"}, want: "dns_error"},
		{name: "network unreachable", err: &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}, want: "network_unreachable"},
		{name: "connection refused", err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, want: "connection_refused"},
		{name: "connection reset", err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}, want: "connection_reset"},
		{name: "TLS", err: x509.UnknownAuthorityError{}, want: "tls_error"},
		{name: "proxy", err: &url.Error{Op: "proxyconnect", URL: "https://proxy-user:authorization-secret@proxy.example", Err: errors.New("proxy rejected client-secret")}, want: "proxy_error"},
		{name: "other", err: errors.New("https://social.example/oauth/token?code=authorization-secret client-secret"), want: "other_transport_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := newOAuthTransportError(test.err).Error()
			assertOAuthLogContains(t, got, "class="+test.want, "detail=")
			assertOAuthLogExcludes(t, got, "authorization-secret", "client-secret", "secret.example", "proxy.example", "social.example", "/oauth/token", "code=")
			if len(got) > 512 {
				t.Fatalf("transport error is unbounded: %d bytes", len(got))
			}
		})
	}
}

func TestOAuthTokenRedirectRejectsDifferentHost(t *testing.T) {
	targetHits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "stolen", "scope": OAuthScopes})
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client := newOAuthClientForBaseURL(t, source.URL, source.Client())
	_, err := client.exchange(context.Background(), url.Values{"code": {"authorization-secret"}, "client_secret": {"client-secret"}})
	if err == nil {
		t.Fatal("exchange error = nil")
	}
	if targetHits != 0 {
		t.Fatalf("cross-host redirect target hits = %d, want 0", targetHits)
	}
	assertOAuthLogExcludes(t, err.Error(), "authorization-secret", "client-secret", target.URL)
}

func TestOAuthTokenRedirectAllowsSameHost(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			http.Redirect(w, r, server.URL+"/oauth/token-final", http.StatusTemporaryRedirect)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-secret", "scope": OAuthScopes})
	}))
	defer server.Close()

	client := newOAuthClientForBaseURL(t, server.URL, server.Client())
	token, err := client.exchange(context.Background(), url.Values{"code": {"authorization-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "access-secret" {
		t.Fatalf("access token = %q", token.AccessToken)
	}
}

func TestOAuthTokenRedirectPreservesConfiguredPolicy(t *testing.T) {
	policyCalls := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/oauth/token-final", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	httpClient := server.Client()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		policyCalls++
		return http.ErrUseLastResponse
	}

	client := newOAuthClientForBaseURL(t, server.URL, httpClient)
	_, err := client.exchange(context.Background(), url.Values{"code": {"authorization-secret"}})
	if err == nil {
		t.Fatal("exchange error = nil")
	}
	if policyCalls != 1 {
		t.Fatalf("configured redirect policy calls = %d, want 1", policyCalls)
	}
}

func TestCallbackLogsSafeTransportFailure(t *testing.T) {
	client, _, _ := newOAuthTestClient(t, "alice", time.Now())
	state := startAuthorization(t, client)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "proxyconnect", URL: r.URL.String() + "?code=authorization-secret", Err: errors.New("proxy rejected client-secret")}
	})}
	logs := captureOAuthLogs(t)
	response := callback(t, client, state, "authorization-secret")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", response.Code)
	}
	assertOAuthLogContains(t, logs.String(), "stage=token_exchange", "class=proxy_error", "detail=")
	assertOAuthLogExcludes(t, logs.String(), state, "authorization-secret", "client-secret", "social.example", "/oauth/token", "code=", "state=")
}

func TestVerifyCredentialsErrorRedactsAccessToken(t *testing.T) {
	client, _, _ := newOAuthTestClient(t, "alice", time.Now())
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Body: io.NopCloser(strings.NewReader(`{"error":"invalid_token","error_description":"access-secret expired"}`)), Header: make(http.Header)}, nil
	})}
	_, err := client.verifyCredentials(context.Background(), "access-secret")
	if err == nil || !strings.Contains(err.Error(), "credential verification") || !strings.Contains(err.Error(), "status=401") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "access-secret") {
		t.Fatalf("error contains access token: %v", err)
	}
}

func TestOAuthResponsesIdentifyInvalidResponseReason(t *testing.T) {
	tests := []struct {
		name string
		body string
		call func(*OAuthClient) error
		want string
	}{
		{name: "token JSON", body: `{`, call: func(client *OAuthClient) error {
			_, err := client.exchange(context.Background(), url.Values{"code": {"authorization-secret"}})
			return err
		}, want: "decode Mastodon OAuth token response"},
		{name: "missing access token", body: `{"scope":"` + OAuthScopes + `"}`, call: func(client *OAuthClient) error {
			_, err := client.exchange(context.Background(), url.Values{"code": {"authorization-secret"}})
			return err
		}, want: "missing access token"},
		{name: "scope mismatch", body: `{"access_token":"access-secret","scope":"profile"}`, call: func(client *OAuthClient) error {
			_, err := client.exchange(context.Background(), url.Values{"code": {"authorization-secret"}})
			return err
		}, want: "scope mismatch"},
		{name: "account JSON", body: `{`, call: func(client *OAuthClient) error {
			_, err := client.verifyCredentials(context.Background(), "access-secret")
			return err
		}, want: "decode Mastodon account response"},
		{name: "missing account", body: `{}`, call: func(client *OAuthClient) error {
			_, err := client.verifyCredentials(context.Background(), "access-secret")
			return err
		}, want: "missing account"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, _, _ := newOAuthTestClient(t, "alice", time.Now())
			client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.body)), Header: make(http.Header)}, nil
			})}
			err := test.call(client)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			assertOAuthLogExcludes(t, err.Error(), "authorization-secret", "access-secret")
		})
	}
}

func TestCallbackLogsSafeTokenExchangeFailure(t *testing.T) {
	client, _, _ := newOAuthTestClient(t, "alice", time.Now())
	state := startAuthorization(t, client)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"error":"invalid_grant","error_description":"authorization-secret client-secret rejected"}`
		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	logs := captureOAuthLogs(t)
	response := callback(t, client, state, "authorization-secret")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", response.Code)
	}
	assertOAuthLogContains(t, logs.String(), "provider=mastodon", "stage=token_exchange", "result=failed", "status=400", "invalid_grant")
	assertOAuthLogExcludes(t, logs.String(), state, "authorization-secret", "client-secret", "code=", "state=")
}

func TestCallbackLogsSafeCredentialVerificationFailure(t *testing.T) {
	client, _, _ := newOAuthTestClient(t, "alice", time.Now())
	state := startAuthorization(t, client)
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth/token" {
			body := `{"access_token":"access-secret","refresh_token":"refresh-secret","scope":"` + OAuthScopes + `"}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		body := `{"error":"invalid_token","error_description":"access-secret expired"}`
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	logs := captureOAuthLogs(t)
	response := callback(t, client, state, "authorization-secret")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", response.Code)
	}
	assertOAuthLogContains(t, logs.String(), "stage=credential_verification", "result=failed", "status=401", "invalid_token")
	assertOAuthLogExcludes(t, logs.String(), state, "authorization-secret", "access-secret", "refresh-secret", "code=", "state=")
}

func TestCallbackLogsSuccessfulLifecycleWithoutSecrets(t *testing.T) {
	client, _, _ := newOAuthTestClient(t, "alice", time.Now())
	state := startAuthorization(t, client)
	logs := captureOAuthLogs(t)
	response := callback(t, client, state, "authorization-secret")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", response.Code)
	}
	assertOAuthLogContains(t, logs.String(), "stage=callback", "result=started", "stage=complete", "result=succeeded")
	assertOAuthLogExcludes(t, logs.String(), state, "authorization-secret", "access-secret", "refresh-secret", "client-secret", "code=", "state=")
}

func captureOAuthLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &logs
}

func assertOAuthLogContains(t *testing.T, output string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(output, value) {
			t.Errorf("log %q missing %q", output, value)
		}
	}
}

func assertOAuthLogExcludes(t *testing.T, output string, values ...string) {
	t.Helper()
	for _, value := range values {
		if value != "" && strings.Contains(output, value) {
			t.Errorf("log contains unsafe value %q: %q", value, output)
		}
	}
}

type recordedForms struct{ token, refresh url.Values }

func newOAuthClientForBaseURL(t *testing.T, baseURL string, httpClient *http.Client) *OAuthClient {
	t.Helper()
	account := normalizeAccount("alice", instanceHost(baseURL))
	client, err := NewOAuthClient(OAuthOptions{
		Scope:         bridgestore.SourceScope{Provider: "mastodon", Account: account},
		Store:         &memoryOAuthStore{},
		HTTPClient:    httpClient,
		BaseURL:       baseURL,
		Account:       account,
		ClientID:      "client",
		ClientSecret:  "client-secret",
		RedirectURL:   "https://bridge.example/oauth/mastodon/callback",
		EncryptionKey: make([]byte, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func newOAuthTestClient(t *testing.T, acct string, now time.Time) (*OAuthClient, *memoryOAuthStore, *recordedForms) {
	t.Helper()
	forms := &recordedForms{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "nostr-bridge" {
			t.Errorf("User-Agent = %q, want %q", got, "nostr-bridge")
		}
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
	sessions       map[string]bridgestore.OAuthSession
	tokens         map[string]bridgestore.OAuthToken
	saveSessionErr error
}

func (s *memoryOAuthStore) SaveOAuthSession(_ context.Context, scope bridgestore.SourceScope, v bridgestore.OAuthSession) error {
	if s.saveSessionErr != nil {
		return s.saveSessionErr
	}
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
func (s *memoryOAuthStore) UpdateOAuthTokenRefreshFailure(_ context.Context, scope bridgestore.SourceScope, id, class string, reauthRequired bool) error {
	key := scope.Provider + "\x00" + scope.Account + "\x00" + id
	v, ok := s.tokens[key]
	if !ok {
		return sql.ErrNoRows
	}
	v.LastRefreshErrorClass = class
	v.ReauthRequired = reauthRequired
	s.tokens[key] = v
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
