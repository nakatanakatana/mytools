package oauth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/secretbox"
	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

var oauthTestScope = bridgestore.SourceScope{Provider: "bluesky", Account: "did:plc:alice"}

func TestStartAuthorizationRetriesDPoPNonceChallenge(t *testing.T) {
	const challengeNonce = "secret-par-nonce"
	var proofs []string
	var assertions []string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validMetadataBody(server.URL)))
		case "/oauth/par":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			proofs = append(proofs, r.Header.Get("DPoP"))
			assertions = append(assertions, r.Form.Get("client_assertion"))
			if len(proofs) == 1 {
				w.Header().Set("DPoP-Nonce", challengeNonce)
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "use_dpop_nonce"})
				return
			}
			w.Header().Set("DPoP-Nonce", "next-par-nonce")
			_ = json.NewEncoder(w).Encode(map[string]any{"request_uri": "urn:request:retry", "expires_in": 600})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	client, _ := newTestClient(t, server.URL, server.Client())

	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	redirect, err := client.StartAuthorization(context.Background(), "alice.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(proofs) != 2 || len(assertions) != 2 {
		t.Fatalf("requests = %d, want 2", len(proofs))
	}
	firstProof, secondProof := jwtClaims(t, proofs[0]), jwtClaims(t, proofs[1])
	if _, ok := firstProof["nonce"]; ok {
		t.Fatalf("first DPoP proof nonce = %#v", firstProof["nonce"])
	}
	if secondProof["nonce"] != challengeNonce || firstProof["jti"] == secondProof["jti"] {
		t.Fatalf("DPoP claims = %#v, %#v", firstProof, secondProof)
	}
	if jwtClaims(t, assertions[0])["jti"] == jwtClaims(t, assertions[1])["jti"] {
		t.Fatal("client assertion jti was reused")
	}
	if !strings.Contains(logs.String(), "DPoP-Nonce header present=true") || strings.Contains(logs.String(), challengeNonce) {
		t.Fatalf("log = %q", logs.String())
	}
	parsed, err := url.Parse(redirect)
	if err != nil || parsed.Query().Get("request_uri") != "urn:request:retry" {
		t.Fatalf("redirect = %q", redirect)
	}
}

func TestStartAuthorizationPushesPKCEAuthenticatedRequestAndStoresEncryptedState(t *testing.T) {
	var gotPAR url.Values
	var gotDPoP string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validMetadataBody(server.URL)))
			return
		case "/oauth/par":
		default:
			t.Fatalf("path = %q, want metadata or /oauth/par", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		gotPAR, gotDPoP = r.Form, r.Header.Get("DPoP")
		w.Header().Set("DPoP-Nonce", "par-nonce")
		_ = json.NewEncoder(w).Encode(map[string]any{"request_uri": "urn:request:one", "expires_in": 600})
	}))
	defer server.Close()

	client, stateStore := newTestClient(t, server.URL, server.Client())
	redirect, err := client.StartAuthorization(context.Background(), "alice.test")
	if err != nil {
		t.Fatal(err)
	}

	if gotPAR.Get("response_type") != "code" || gotPAR.Get("code_challenge_method") != "S256" || gotPAR.Get("login_hint") != "alice.test" {
		t.Fatalf("PAR form = %#v", gotPAR)
	}
	assertRequestedScopes(t, gotPAR.Get("scope"))
	if gotPAR.Get("client_assertion_type") != clientAssertionType || gotPAR.Get("client_assertion") == "" || gotDPoP == "" {
		t.Fatalf("PAR authentication missing: form=%#v DPoP=%q", gotPAR, gotDPoP)
	}
	parsed, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/oauth/authorize" || parsed.Query().Get("request_uri") != "urn:request:one" || parsed.Query().Get("client_id") != client.clientID {
		t.Fatalf("redirect = %q", redirect)
	}
	if got := jwtClaims(t, gotDPoP)["htu"]; got != server.URL+"/oauth/par" {
		t.Fatalf("DPoP htu = %#v", got)
	}

	session, err := stateStore.OAuthSessionByState(context.Background(), oauthTestScope, gotPAR.Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	if string(session.EncryptedPayload) == "" || string(session.EncryptedPayload) == gotPAR.Get("code_challenge") || session.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("stored session = %#v", session)
	}
	var payload sessionPayload
	if err := client.decryptJSON(session.EncryptedPayload, &payload); err != nil {
		t.Fatal(err)
	}
	wantChallenge := sha256.Sum256([]byte(payload.CodeVerifier))
	if got, want := gotPAR.Get("code_challenge"), base64.RawURLEncoding.EncodeToString(wantChallenge[:]); got != want {
		t.Fatalf("code_challenge = %q, want S256(%q) = %q", got, payload.CodeVerifier, want)
	}
	assertClientAssertion(t, gotPAR.Get("client_assertion"), &client.clientSigningKey.PublicKey, client.clientID, server.URL)
}

func TestHandleCallbackExchangesCodeAndSavesEncryptedTokensWithRefreshState(t *testing.T) {
	fixedNow := time.Unix(2_000_000_000, 0)
	observer := &clientObserverRecorder{}
	var tokenForm url.Values
	var tokenDPoPs []string
	var metadataCalls int
	var state string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			metadataCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validMetadataBody(server.URL)))
		case "/oauth/par":
			_ = r.ParseForm()
			state = r.Form.Get("state")
			w.Header().Set("DPoP-Nonce", "par-nonce")
			_ = json.NewEncoder(w).Encode(map[string]any{"request_uri": "urn:request:one"})
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			tokenForm = r.Form
			tokenDPoPs = append(tokenDPoPs, r.Header.Get("DPoP"))
			if tokenDPoPs[len(tokenDPoPs)-1] == "" {
				t.Fatal("token request missing DPoP")
			}
			if len(tokenDPoPs) == 1 {
				w.Header().Set("DPoP-Nonce", "code-exchange-challenge")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "use_dpop_nonce"})
				return
			}
			w.Header().Set("DPoP-Nonce", "token-nonce")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-secret", "refresh_token": "refresh-secret", "sub": "did:plc:alice", "scope": "atproto", "expires_in": 3600})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client, stateStore := newTestClient(t, server.URL, server.Client())
	client.now = func() time.Time { return fixedNow }
	client.observer = observer
	client.refreshPeriod = 30 * 24 * time.Hour
	if _, err := client.StartAuthorization(context.Background(), "alice.test"); err != nil {
		t.Fatal(err)
	}
	recordingStore := &recordingOAuthStore{OAuthStore: stateStore}
	client.store = recordingStore
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/oauth/callback?state="+url.QueryEscape(state)+"&code=authorization-code&iss="+url.QueryEscape(server.URL), nil)
	client.HandleCallback(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if tokenForm.Get("grant_type") != "authorization_code" || tokenForm.Get("code") != "authorization-code" || tokenForm.Get("code_verifier") == "" || tokenForm.Get("client_assertion") == "" {
		t.Fatalf("token form = %#v", tokenForm)
	}
	assertClientAssertion(t, tokenForm.Get("client_assertion"), &client.clientSigningKey.PublicKey, client.clientID, server.URL)
	if len(tokenDPoPs) != 2 || jwtClaims(t, tokenDPoPs[1])["nonce"] != "code-exchange-challenge" {
		t.Fatalf("token DPoP proofs = %#v", tokenDPoPs)
	}
	if got := jwtClaims(t, tokenDPoPs[1])["htu"]; got != server.URL+"/oauth/token" {
		t.Fatalf("DPoP htu = %#v", got)
	}
	token, err := stateStore.OAuthTokenByAccountDID(context.Background(), oauthTestScope, "did:plc:alice")
	if err != nil {
		t.Fatal(err)
	}
	if string(token.EncryptedPayload) == "access-secret" || len(token.EncryptedPayload) == 0 {
		t.Fatalf("token payload encryption check failed: payload length %d", len(token.EncryptedPayload))
	}
	if recordingStore.saveCalls != 1 {
		t.Fatalf("SaveOAuthToken calls = %d, want 1", recordingStore.saveCalls)
	}
	if token.UpdatedAt != fixedNow.Unix() || token.LastRefreshAt != fixedNow.Unix() || token.ReauthRequired || token.LastRefreshErrorClass != "" {
		t.Fatalf(
			"stored OAuth refresh state = updated %d refresh %d reauth %t class %q",
			token.UpdatedAt, token.LastRefreshAt, token.ReauthRequired, token.LastRefreshErrorClass,
		)
	}
	key := sha256.Sum256([]byte("test encryption key"))
	box, err := secretbox.New(key[:])
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := box.Open(token.EncryptedPayload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plaintext), "access-secret") {
		t.Fatalf("shared box did not open OAuth payload: %q", plaintext)
	}
	loaded, err := client.TokenByAccountDID(context.Background(), "did:plc:alice")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AccessToken != "access-secret" || loaded.RefreshToken != "refresh-secret" || loaded.DPoPNonce != "token-nonce" || loaded.DPoPKey == nil {
		t.Fatalf("loaded OAuth token = %#v", loaded)
	}
	if loaded.Expiry.IsZero() {
		t.Fatal("loaded OAuth token has no expiry")
	}
	if _, err := stateStore.OAuthSessionByState(context.Background(), oauthTestScope, state); err == nil {
		t.Fatal("OAuth session remains after callback")
	}
	if metadataCalls != 2 {
		t.Fatalf("authorization metadata requests = %d, want 2 before local callback status refresh", metadataCalls)
	}
	if len(observer.successes) != 1 || observer.successes[0] != RefreshReasonAuthorizationCode {
		t.Fatalf("callback refresh successes = %#v, want authorization_code", observer.successes)
	}
	if len(observer.failures) != 0 {
		t.Fatalf("callback refresh failures = %#v, want none", observer.failures)
	}
	if len(observer.statuses) != 1 {
		t.Fatalf("callback status updates = %d, want 1", len(observer.statuses))
	}
	status := observer.statuses[0]
	if !status.AuthorizationAvailable || !status.AccessTokenValid || status.ReauthRequired ||
		status.LastRefreshErrorClass != "" ||
		!status.AccessTokenExpiry.Equal(fixedNow.Add(time.Hour)) ||
		!status.LastRefreshSucceededAt.Equal(fixedNow) ||
		!status.NextMaintenanceRefresh.Equal(fixedNow.Add(30*24*time.Hour)) {
		t.Fatalf("callback authorization status = %#v", status)
	}
}

func TestHandleCallbackPublishesBoundedAuthorizationCodeFailure(t *testing.T) {
	const (
		stateSecret       = "oauth-state-fixture"
		codeSecret        = "authorization-code-fixture"
		descriptionSecret = "remote-response-description-fixture"
	)
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validMetadataBody(server.URL)))
		case "/oauth/token":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":             "invalid_grant",
				"error_description": descriptionSecret,
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client, store := newTestClient(t, server.URL, server.Client())
	observer := &clientObserverRecorder{}
	client.observer = observer
	payload, err := json.Marshal(sessionPayload{
		CodeVerifier: "pkce-verifier-fixture",
		Issuer:       server.URL,
		DPoPKey:      newStatusTestJWK(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := client.encrypt(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveOAuthSession(context.Background(), oauthTestScope, bridgestore.OAuthSession{
		State:            stateSecret,
		EncryptedPayload: encrypted,
		ExpiresAt:        time.Now().Add(time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodGet,
		"/oauth/callback?state="+url.QueryEscape(stateSecret)+
			"&code="+url.QueryEscape(codeSecret)+
			"&iss="+url.QueryEscape(server.URL),
		nil,
	)
	client.HandleCallback(recorder, request)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("callback status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if len(observer.successes) != 0 ||
		len(observer.failures) != 1 ||
		observer.failures[0] != (clientObserverFailure{
			reason: RefreshReasonAuthorizationCode,
			class:  RefreshErrorInvalidGrant,
		}) {
		t.Fatalf("authorization-code refresh events: successes=%#v failures=%#v", observer.successes, observer.failures)
	}
	exposed := recorder.Body.String() + fmt.Sprint(observer.failures)
	for _, secret := range []string{stateSecret, codeSecret, descriptionSecret} {
		if strings.Contains(exposed, secret) {
			t.Fatalf("callback failure exposed secret %q: %q", secret, exposed)
		}
	}
}

func TestHandleCallbackRejectsTokenSubjectOutsideConfiguredAccount(t *testing.T) {
	const (
		stateSecret        = "oauth-state-subject-fixture"
		codeSecret         = "authorization-code-subject-fixture"
		accessTokenSecret  = "access-token-subject-fixture"
		refreshTokenSecret = "refresh-token-subject-fixture"
		otherDIDSecret     = "did:plc:other-account-fixture"
	)
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validMetadataBody(server.URL)))
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  accessTokenSecret,
				"refresh_token": refreshTokenSecret,
				"sub":           otherDIDSecret,
				"scope":         "atproto",
				"expires_in":    3600,
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client, store := newTestClient(t, server.URL, server.Client())
	observer := &clientObserverRecorder{}
	client.observer = observer
	payload, err := json.Marshal(sessionPayload{
		CodeVerifier: "pkce-verifier-subject-fixture",
		Issuer:       server.URL,
		DPoPKey:      newStatusTestJWK(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := client.encrypt(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveOAuthSession(context.Background(), oauthTestScope, bridgestore.OAuthSession{
		State:            stateSecret,
		EncryptedPayload: encrypted,
		ExpiresAt:        time.Now().Add(time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	recordingStore := &recordingOAuthStore{OAuthStore: store}
	client.store = recordingStore

	recorder := httptest.NewRecorder()
	client.HandleCallback(
		recorder,
		httptest.NewRequest(
			http.MethodGet,
			"/oauth/callback?state="+url.QueryEscape(stateSecret)+
				"&code="+url.QueryEscape(codeSecret)+
				"&iss="+url.QueryEscape(server.URL),
			nil,
		),
	)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("callback status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if recordingStore.saveCalls != 0 {
		t.Fatalf("SaveOAuthToken calls = %d, want 0", recordingStore.saveCalls)
	}
	for _, accountDID := range []string{oauthTestScope.Account, otherDIDSecret} {
		if _, err := store.OAuthTokenByAccountDID(context.Background(), oauthTestScope, accountDID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("OAuth token for %q error = %v, want no row", accountDID, err)
		}
	}
	if len(observer.successes) != 0 || len(observer.statuses) != 0 ||
		len(observer.failures) != 1 ||
		observer.failures[0] != (clientObserverFailure{
			reason: RefreshReasonAuthorizationCode,
			class:  RefreshErrorProtocol,
		}) {
		t.Fatalf(
			"authorization-code events: successes=%#v failures=%#v statuses=%#v",
			observer.successes,
			observer.failures,
			observer.statuses,
		)
	}
	exposed := recorder.Body.String() + fmt.Sprint(observer.failures)
	for _, secret := range []string{
		stateSecret,
		codeSecret,
		accessTokenSecret,
		refreshTokenSecret,
		oauthTestScope.Account,
		otherDIDSecret,
	} {
		if strings.Contains(exposed, secret) {
			t.Fatalf("callback failure exposed secret %q: %q", secret, exposed)
		}
	}
}

func TestTokenByAccountDIDRefreshesExpiredTokenAndPersistsRotation(t *testing.T) {
	fixedNow := time.Unix(2_000_000_000, 0)
	observer := &clientObserverRecorder{}
	var form url.Values
	var tokenDPoPs []string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validMetadataBody(server.URL)))
			return
		}
		if r.URL.Path != "/oauth/token" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		form = r.Form
		tokenDPoPs = append(tokenDPoPs, r.Header.Get("DPoP"))
		if len(tokenDPoPs) == 1 {
			w.Header().Set("DPoP-Nonce", "refresh-challenge")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "use_dpop_nonce"})
			return
		}
		w.Header().Set("DPoP-Nonce", "refreshed-nonce")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "scope": "atproto", "expires_in": 3600})
	}))
	defer server.Close()
	client, store := newTestClient(t, server.URL, server.Client())
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(tokenPayload{AccessToken: "old-access", RefreshToken: "old-refresh", Scope: "atproto", DPoPKey: privateJWK(key), DPoPNonce: "old-nonce", Expiry: fixedNow.Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := client.encrypt(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveOAuthToken(context.Background(), oauthTestScope, bridgestore.OAuthToken{
		AccountDID:            "did:plc:alice",
		EncryptedPayload:      encrypted,
		UpdatedAt:             fixedNow.Add(-24 * time.Hour).Unix(),
		LastRefreshAt:         fixedNow.Add(-24 * time.Hour).Unix(),
		ReauthRequired:        false,
		LastRefreshErrorClass: string(RefreshErrorRateLimit),
	}); err != nil {
		t.Fatal(err)
	}
	recordingStore := &recordingOAuthStore{OAuthStore: store}
	encryptionKey := sha256.Sum256([]byte("test encryption key"))
	restarted, err := NewClient(Options{
		Scope: oauthTestScope, Store: recordingStore, HTTPClient: server.Client(),
		AuthorizationServerURL: server.URL, ClientID: client.clientID, RedirectURL: client.redirectURL,
		ClientSigningKey: client.clientSigningKey, EncryptionKey: encryptionKey[:],
		Now: func() time.Time { return fixedNow }, Observer: observer,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.TokenByAccountDID(context.Background(), "did:plc:alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" || got.DPoPNonce != "refreshed-nonce" {
		t.Fatalf("refreshed token = %#v", got)
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "old-refresh" || form.Get("client_assertion") == "" {
		t.Fatalf("refresh form = %#v", form)
	}
	assertClientAssertion(t, form.Get("client_assertion"), &client.clientSigningKey.PublicKey, client.clientID, server.URL)
	if len(tokenDPoPs) != 2 || jwtClaims(t, tokenDPoPs[1])["nonce"] != "refresh-challenge" {
		t.Fatalf("refresh DPoP proofs = %#v", tokenDPoPs)
	}
	if got := jwtClaims(t, tokenDPoPs[1])["htu"]; got != server.URL+"/oauth/token" {
		t.Fatalf("DPoP htu = %#v", got)
	}
	if recordingStore.saveCalls != 1 {
		t.Fatalf("SaveOAuthToken calls = %d, want 1", recordingStore.saveCalls)
	}
	stored, err := store.OAuthTokenByAccountDID(context.Background(), oauthTestScope, "did:plc:alice")
	if err != nil {
		t.Fatal(err)
	}
	if stored.UpdatedAt != fixedNow.Unix() || stored.LastRefreshAt != fixedNow.Unix() || stored.ReauthRequired || stored.LastRefreshErrorClass != "" {
		t.Fatalf(
			"stored OAuth refresh state = updated %d refresh %d reauth %t class %q",
			stored.UpdatedAt, stored.LastRefreshAt, stored.ReauthRequired, stored.LastRefreshErrorClass,
		)
	}
	var storedPayload tokenPayload
	if err := restarted.decryptJSON(stored.EncryptedPayload, &storedPayload); err != nil {
		t.Fatal(err)
	}
	if storedPayload.AccessToken != "new-access" || storedPayload.RefreshToken != "new-refresh" || storedPayload.DPoPNonce != "refreshed-nonce" || !storedPayload.Expiry.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("stored refreshed payload = %#v", storedPayload)
	}
	if len(observer.successes) != 1 || observer.successes[0] != RefreshReasonOnDemand {
		t.Fatalf("on-demand refresh successes = %#v, want on_demand", observer.successes)
	}
	if len(observer.failures) != 0 {
		t.Fatalf("on-demand refresh failures = %#v, want none", observer.failures)
	}
}

func TestOnDemandRefreshMovesMaintenanceOrigin(t *testing.T) {
	fixedNow := time.Unix(2_000_000_000, 0)
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validMetadataBody(server.URL)))
		case "/oauth/token":
			w.Header().Set("DPoP-Nonce", "rotated-nonce")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "rotated-access", "refresh_token": "rotated-refresh",
				"scope": "atproto", "expires_in": 3600,
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client, store := newTestClient(t, server.URL, server.Client())
	client.now = func() time.Time { return fixedNow }
	saveStatusTestToken(t, client, store, tokenPayload{
		AccessToken: "expired-access", RefreshToken: "refresh-token", Scope: "atproto",
		DPoPKey: newStatusTestJWK(t), Expiry: fixedNow.Add(-time.Minute),
	}, bridgestore.OAuthToken{
		AccountDID: "did:plc:alice", UpdatedAt: fixedNow.Add(-31 * 24 * time.Hour).Unix(),
		LastRefreshAt: fixedNow.Add(-31 * 24 * time.Hour).Unix(),
	})
	recordingStore := &recordingOAuthStore{OAuthStore: store}
	client.store = recordingStore

	if _, err := client.TokenByAccountDID(context.Background(), "did:plc:alice"); err != nil {
		t.Fatal(err)
	}
	status, err := client.AuthorizationStatus(context.Background(), "did:plc:alice", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if recordingStore.saveCalls != 1 {
		t.Fatalf("SaveOAuthToken calls = %d, want 1", recordingStore.saveCalls)
	}
	if recordingStore.lastSaved.LastRefreshAt != fixedNow.Unix() {
		t.Fatalf("saved last refresh = %d, want %d", recordingStore.lastSaved.LastRefreshAt, fixedNow.Unix())
	}
	if !status.LastRefreshSucceededAt.Equal(fixedNow) || !status.NextMaintenanceRefresh.Equal(fixedNow.Add(30*24*time.Hour)) {
		t.Fatalf("authorization status after on-demand refresh = %#v", status)
	}
}

type recordingOAuthStore struct {
	bridgestore.OAuthStore
	saveCalls    int
	lastSaved    bridgestore.OAuthToken
	failureCalls int
}

type clientObserverFailure struct {
	reason RefreshReason
	class  RefreshErrorClass
}

type clientObserverRecorder struct {
	successes []RefreshReason
	failures  []clientObserverFailure
	statuses  []Status
}

func (o *clientObserverRecorder) RefreshSucceeded(_ time.Time, reason RefreshReason) {
	o.successes = append(o.successes, reason)
}

func (o *clientObserverRecorder) RefreshFailed(_ time.Time, reason RefreshReason, class RefreshErrorClass) {
	o.failures = append(o.failures, clientObserverFailure{reason: reason, class: class})
}

func (o *clientObserverRecorder) AuthorizationStatusChanged(_ time.Time, status Status) {
	o.statuses = append(o.statuses, status)
}

func (s *recordingOAuthStore) SaveOAuthToken(ctx context.Context, scope bridgestore.SourceScope, token bridgestore.OAuthToken) error {
	s.saveCalls++
	s.lastSaved = token
	return s.OAuthStore.SaveOAuthToken(ctx, scope, token)
}

func (s *recordingOAuthStore) UpdateOAuthTokenRefreshFailure(ctx context.Context, scope bridgestore.SourceScope, accountDID, class string, reauthRequired bool) error {
	s.failureCalls++
	return s.OAuthStore.UpdateOAuthTokenRefreshFailure(ctx, scope, accountDID, class, reauthRequired)
}

func TestRefreshFailureClassificationPreservesTokenMaterial(t *testing.T) {
	fixedNow := time.Unix(2_000_000_000, 0)
	tests := []struct {
		name           string
		metadataStatus int
		status         int
		body           string
		transportErr   error
		wantClass      RefreshErrorClass
		wantPermanent  bool
	}{
		{name: "timeout", transportErr: context.DeadlineExceeded, wantClass: RefreshErrorTimeout},
		{name: "connection", transportErr: errors.New("connection unavailable"), wantClass: RefreshErrorConnection},
		{name: "rate limit", status: http.StatusTooManyRequests, body: `{"error":"slow_down","error_description":"retry response secret"}`, wantClass: RefreshErrorRateLimit},
		{name: "server", status: http.StatusInternalServerError, body: `{"error":"server_error","error_description":"server response secret"}`, wantClass: RefreshErrorServer},
		{name: "discovery rate limit", metadataStatus: http.StatusTooManyRequests, body: `{"error":"slow_down","error_description":"discovery retry response secret"}`, wantClass: RefreshErrorRateLimit},
		{name: "discovery server", metadataStatus: http.StatusBadGateway, body: `{"error":"server_error","error_description":"discovery server response secret"}`, wantClass: RefreshErrorServer},
		{name: "invalid grant", status: http.StatusBadRequest, body: `{"error":"invalid_grant","error_description":"grant response secret"}`, wantClass: RefreshErrorInvalidGrant, wantPermanent: true},
		{name: "other client error", status: http.StatusUnauthorized, body: `{"error":"invalid_client","error_description":"client response secret"}`, wantClass: RefreshErrorProtocol, wantPermanent: true},
		{name: "malformed success", status: http.StatusOK, body: `{"access_token":`, wantClass: RefreshErrorProtocol, wantPermanent: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			issuer := "https://issuer.example"
			httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch request.URL.Path {
				case "/.well-known/oauth-authorization-server":
					if test.metadataStatus != 0 {
						return oauthTestResponse(test.metadataStatus, test.body), nil
					}
					return oauthTestResponse(http.StatusOK, validMetadataBody(issuer)), nil
				case "/oauth/token":
					if test.transportErr != nil {
						return nil, test.transportErr
					}
					return oauthTestResponse(test.status, test.body), nil
				default:
					t.Fatalf("unexpected path %q", request.URL.Path)
					return nil, errors.New("unexpected request")
				}
			})}
			client, store := newTestClient(t, issuer, httpClient)
			client.now = func() time.Time { return fixedNow }
			observer := &clientObserverRecorder{}
			client.observer = observer
			before := saveStatusTestToken(t, client, store, tokenPayload{
				AccessToken: "old-access", RefreshToken: "old-refresh", Scope: "atproto",
				DPoPKey: newStatusTestJWK(t), DPoPNonce: "old-nonce", Expiry: fixedNow.Add(-time.Minute),
			}, bridgestore.OAuthToken{
				AccountDID: "did:plc:alice", UpdatedAt: fixedNow.Add(-48 * time.Hour).Unix(),
				LastRefreshAt: fixedNow.Add(-24 * time.Hour).Unix(),
			})
			recordingStore := &recordingOAuthStore{OAuthStore: store}
			client.store = recordingStore

			_, err := client.TokenByAccountDID(context.Background(), before.AccountDID)
			var refreshErr *RefreshError
			if !errors.As(err, &refreshErr) {
				t.Fatalf("error = %T %v, want *RefreshError", err, err)
			}
			if refreshErr.Class != test.wantClass || refreshErr.ReauthRequired != test.wantPermanent {
				t.Fatalf("refresh error = %#v, want class %q permanent %t", refreshErr, test.wantClass, test.wantPermanent)
			}
			if recordingStore.saveCalls != 0 || recordingStore.failureCalls != 1 {
				t.Fatalf("store calls: save=%d failure=%d, want 0/1", recordingStore.saveCalls, recordingStore.failureCalls)
			}
			after, err := store.OAuthTokenByAccountDID(context.Background(), oauthTestScope, before.AccountDID)
			if err != nil {
				t.Fatal(err)
			}
			assertRefreshFailurePreservedToken(t, before, after, test.wantClass, test.wantPermanent)
			if len(observer.successes) != 0 ||
				len(observer.failures) != 1 ||
				observer.failures[0] != (clientObserverFailure{reason: RefreshReasonOnDemand, class: test.wantClass}) {
				t.Fatalf("on-demand refresh events: successes=%#v failures=%#v", observer.successes, observer.failures)
			}
			for _, secret := range []string{"old-access", "old-refresh", "old-nonce", "retry response secret", "server response secret", "grant response secret", "client response secret"} {
				if strings.Contains(fmt.Sprint(observer.failures), secret) {
					t.Fatalf("on-demand refresh event exposed secret %q: %#v", secret, observer.failures)
				}
			}
		})
	}
}

func TestAuthorizationFailureClassificationPreservesTokenMaterial(t *testing.T) {
	fixedNow := time.Unix(2_000_000_000, 0)
	tests := []struct {
		name       string
		payload    *tokenPayload
		ciphertext []byte
		wantClass  RefreshErrorClass
	}{
		{
			name: "missing refresh token",
			payload: &tokenPayload{
				AccessToken: "old-access", Scope: "atproto", DPoPKey: newStatusTestJWK(t),
				Expiry: fixedNow.Add(-time.Minute),
			},
			wantClass: RefreshErrorMissingRefreshToken,
		},
		{name: "decrypt failure", ciphertext: []byte("invalid encrypted payload"), wantClass: RefreshErrorDecrypt},
		{
			name: "invalid DPoP key",
			payload: &tokenPayload{
				AccessToken: "old-access", RefreshToken: "old-refresh", Scope: "atproto",
				DPoPKey: jwk{Kty: "EC", Crv: "P-256", X: "invalid", Y: "invalid", D: "invalid"},
				Expiry:  fixedNow.Add(-time.Minute),
			},
			wantClass: RefreshErrorDPoPKey,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				t.Fatalf("authorization failure made HTTP request to %q", request.URL)
				return nil, errors.New("unexpected request")
			})}
			client, store := newTestClient(t, "https://issuer.example", httpClient)
			client.now = func() time.Time { return fixedNow }
			before := bridgestore.OAuthToken{
				AccountDID: "did:plc:alice", UpdatedAt: fixedNow.Add(-48 * time.Hour).Unix(),
				LastRefreshAt: fixedNow.Add(-24 * time.Hour).Unix(),
			}
			if test.payload != nil {
				before = saveStatusTestToken(t, client, store, *test.payload, before)
			} else {
				before.EncryptedPayload = append([]byte(nil), test.ciphertext...)
				if err := store.SaveOAuthToken(context.Background(), oauthTestScope, before); err != nil {
					t.Fatal(err)
				}
			}
			recordingStore := &recordingOAuthStore{OAuthStore: store}
			client.store = recordingStore

			_, err := client.TokenByAccountDID(context.Background(), before.AccountDID)
			var refreshErr *RefreshError
			if !errors.As(err, &refreshErr) {
				t.Fatalf("error = %T %v, want *RefreshError", err, err)
			}
			if refreshErr.Class != test.wantClass || !refreshErr.ReauthRequired {
				t.Fatalf("refresh error = %#v, want class %q permanent", refreshErr, test.wantClass)
			}
			if recordingStore.saveCalls != 0 || recordingStore.failureCalls != 1 {
				t.Fatalf("store calls: save=%d failure=%d, want 0/1", recordingStore.saveCalls, recordingStore.failureCalls)
			}
			after, err := store.OAuthTokenByAccountDID(context.Background(), oauthTestScope, before.AccountDID)
			if err != nil {
				t.Fatal(err)
			}
			assertRefreshFailurePreservedToken(t, before, after, test.wantClass, true)
		})
	}
}

func TestAuthorizationFailureClassificationForMaintenance(t *testing.T) {
	fixedNow := time.Unix(2_000_000_000, 0)
	tests := []struct {
		name       string
		payload    *tokenPayload
		ciphertext []byte
		wantClass  RefreshErrorClass
	}{
		{
			name: "missing refresh token",
			payload: &tokenPayload{
				AccessToken: "old-access", Scope: "atproto", DPoPKey: newStatusTestJWK(t),
				Expiry: fixedNow.Add(time.Hour),
			},
			wantClass: RefreshErrorMissingRefreshToken,
		},
		{name: "decrypt failure", ciphertext: []byte("invalid encrypted payload"), wantClass: RefreshErrorDecrypt},
		{
			name: "invalid DPoP key",
			payload: &tokenPayload{
				AccessToken: "old-access", RefreshToken: "old-refresh", Scope: "atproto",
				DPoPKey: jwk{Kty: "EC", Crv: "P-256", X: "invalid", Y: "invalid", D: "invalid"},
				Expiry:  fixedNow.Add(time.Hour),
			},
			wantClass: RefreshErrorDPoPKey,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				t.Fatalf("maintenance authorization failure made HTTP request to %q", request.URL)
				return nil, errors.New("unexpected request")
			})}
			client, store := newTestClient(t, "https://issuer.example", httpClient)
			client.now = func() time.Time { return fixedNow }
			before := bridgestore.OAuthToken{
				AccountDID: "did:plc:alice", UpdatedAt: fixedNow.Add(-48 * time.Hour).Unix(),
				LastRefreshAt: fixedNow.Add(-24 * time.Hour).Unix(),
			}
			if test.payload != nil {
				before = saveStatusTestToken(t, client, store, *test.payload, before)
			} else {
				before.EncryptedPayload = append([]byte(nil), test.ciphertext...)
				if err := store.SaveOAuthToken(context.Background(), oauthTestScope, before); err != nil {
					t.Fatal(err)
				}
			}
			recordingStore := &recordingOAuthStore{OAuthStore: store}
			client.store = recordingStore

			_, err := client.RefreshIfDue(context.Background(), before.AccountDID, time.Hour)
			var refreshErr *RefreshError
			if !errors.As(err, &refreshErr) {
				t.Fatalf("error = %T %v, want *RefreshError", err, err)
			}
			if refreshErr.Class != test.wantClass || !refreshErr.ReauthRequired {
				t.Fatalf("refresh error = %#v, want class %q permanent", refreshErr, test.wantClass)
			}
			if recordingStore.saveCalls != 0 || recordingStore.failureCalls != 1 {
				t.Fatalf("store calls: save=%d failure=%d, want 0/1", recordingStore.saveCalls, recordingStore.failureCalls)
			}
			after, err := store.OAuthTokenByAccountDID(context.Background(), oauthTestScope, before.AccountDID)
			if err != nil {
				t.Fatal(err)
			}
			assertRefreshFailurePreservedToken(t, before, after, test.wantClass, true)
		})
	}
}

func TestAuthorizationFailureSuppressesRepeatedDecryptAndDPoPUpdates(t *testing.T) {
	fixedNow := time.Unix(2_000_000_000, 0)
	tests := []struct {
		name       string
		payload    *tokenPayload
		ciphertext []byte
		wantClass  RefreshErrorClass
	}{
		{name: "prior decrypt failure", ciphertext: []byte("invalid encrypted payload"), wantClass: RefreshErrorDecrypt},
		{
			name: "prior DPoP key failure",
			payload: &tokenPayload{
				AccessToken: "old-access", RefreshToken: "old-refresh", Scope: "atproto",
				DPoPKey: jwk{Kty: "EC", Crv: "P-256", X: "invalid", Y: "invalid", D: "invalid"},
				Expiry:  fixedNow.Add(-time.Minute),
			},
			wantClass: RefreshErrorDPoPKey,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var httpCalls int32
			httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				atomic.AddInt32(&httpCalls, 1)
				return nil, errors.New("unexpected request")
			})}
			client, store := newTestClient(t, "https://issuer.example", httpClient)
			client.now = func() time.Time { return fixedNow }
			before := bridgestore.OAuthToken{
				AccountDID: "did:plc:alice", UpdatedAt: fixedNow.Add(-48 * time.Hour).Unix(),
				LastRefreshAt: fixedNow.Add(-24 * time.Hour).Unix(),
			}
			if test.payload != nil {
				before = saveStatusTestToken(t, client, store, *test.payload, before)
			} else {
				before.EncryptedPayload = append([]byte(nil), test.ciphertext...)
				if err := store.SaveOAuthToken(context.Background(), oauthTestScope, before); err != nil {
					t.Fatal(err)
				}
			}
			recordingStore := &recordingOAuthStore{OAuthStore: store}
			client.store = recordingStore

			_, firstErr := client.TokenByAccountDID(context.Background(), before.AccountDID)
			assertPermanentRefreshError(t, firstErr, test.wantClass)
			if recordingStore.saveCalls != 0 || recordingStore.failureCalls != 1 {
				t.Fatalf("first call store mutations: save=%d failure=%d, want 0/1", recordingStore.saveCalls, recordingStore.failureCalls)
			}

			recordingStore.saveCalls = 0
			recordingStore.failureCalls = 0
			_, secondErr := client.TokenByAccountDID(context.Background(), before.AccountDID)
			assertPermanentRefreshError(t, secondErr, test.wantClass)
			if got := atomic.LoadInt32(&httpCalls); got != 0 {
				t.Fatalf("HTTP calls = %d, want 0", got)
			}
			if recordingStore.saveCalls != 0 || recordingStore.failureCalls != 0 {
				t.Fatalf("second call store mutations: save=%d failure=%d, want 0/0", recordingStore.saveCalls, recordingStore.failureCalls)
			}
			after, err := store.OAuthTokenByAccountDID(context.Background(), oauthTestScope, before.AccountDID)
			if err != nil {
				t.Fatal(err)
			}
			assertRefreshFailurePreservedToken(t, before, after, test.wantClass, true)
		})
	}
}

func TestAuthorizationFailureSuppressesSubsequentOnDemandRefresh(t *testing.T) {
	fixedNow := time.Unix(2_000_000_000, 0)
	var httpCalls int32
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		atomic.AddInt32(&httpCalls, 1)
		return nil, errors.New("unexpected request")
	})}
	client, store := newTestClient(t, "https://issuer.example", httpClient)
	client.now = func() time.Time { return fixedNow }
	saveStatusTestToken(t, client, store, tokenPayload{
		AccessToken: "expired-access", RefreshToken: "invalid-refresh", Scope: "atproto",
		DPoPKey: newStatusTestJWK(t), Expiry: fixedNow.Add(-time.Minute),
	}, bridgestore.OAuthToken{
		AccountDID:            "did:plc:alice",
		UpdatedAt:             fixedNow.Add(-24 * time.Hour).Unix(),
		LastRefreshAt:         fixedNow.Add(-24 * time.Hour).Unix(),
		ReauthRequired:        true,
		LastRefreshErrorClass: string(RefreshErrorInvalidGrant),
	})
	recordingStore := &recordingOAuthStore{OAuthStore: store}
	client.store = recordingStore

	_, err := client.TokenByAccountDID(context.Background(), "did:plc:alice")
	var refreshErr *RefreshError
	if !errors.As(err, &refreshErr) {
		t.Fatalf("error = %T %v, want *RefreshError", err, err)
	}
	if refreshErr.Class != RefreshErrorInvalidGrant || !refreshErr.ReauthRequired {
		t.Fatalf("refresh error = %#v, want invalid_grant permanent", refreshErr)
	}
	if got := atomic.LoadInt32(&httpCalls); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0", got)
	}
	if recordingStore.saveCalls != 0 || recordingStore.failureCalls != 0 {
		t.Fatalf("store mutation calls: save=%d failure=%d, want 0/0", recordingStore.saveCalls, recordingStore.failureCalls)
	}
}

func assertPermanentRefreshError(t *testing.T, err error, wantClass RefreshErrorClass) {
	t.Helper()
	var refreshErr *RefreshError
	if !errors.As(err, &refreshErr) {
		t.Fatalf("error = %T %v, want *RefreshError", err, err)
	}
	if refreshErr.Class != wantClass || !refreshErr.ReauthRequired {
		t.Fatalf("refresh error = %#v, want class %q permanent", refreshErr, wantClass)
	}
}

func TestRefreshFailureDoesNotExposeSecrets(t *testing.T) {
	const (
		accessSecret      = "access-fixture-secret"
		refreshSecret     = "refresh-fixture-secret"
		nonceSecret       = "nonce-fixture-secret"
		descriptionSecret = "description-fixture-secret"
	)
	issuer := "https://issuer.example"
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/.well-known/oauth-authorization-server" {
			return oauthTestResponse(http.StatusOK, validMetadataBody(issuer)), nil
		}
		return oauthTestResponse(http.StatusBadRequest, `{"error":"invalid_grant","error_description":"`+descriptionSecret+` `+accessSecret+` `+refreshSecret+` `+nonceSecret+`"}`), nil
	})}
	client, store := newTestClient(t, issuer, httpClient)
	fixedNow := time.Unix(2_000_000_000, 0)
	client.now = func() time.Time { return fixedNow }
	payload := tokenPayload{
		AccessToken: accessSecret, RefreshToken: refreshSecret, Scope: "atproto",
		DPoPKey: newStatusTestJWK(t), DPoPNonce: nonceSecret, Expiry: fixedNow.Add(-time.Minute),
	}
	saveStatusTestToken(t, client, store, payload, bridgestore.OAuthToken{AccountDID: "did:plc:alice"})
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	_, err := client.TokenByAccountDID(context.Background(), "did:plc:alice")
	var refreshErr *RefreshError
	if !errors.As(err, &refreshErr) {
		t.Fatalf("error = %T %v, want *RefreshError", err, err)
	}
	combined := err.Error() + "\n" + logs.String()
	for _, secret := range []string{accessSecret, refreshSecret, nonceSecret, descriptionSecret, payload.DPoPKey.D} {
		if strings.Contains(combined, secret) {
			t.Fatalf("error or log exposed secret %q: %s", secret, combined)
		}
	}
}

func oauthTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func assertRefreshFailurePreservedToken(t *testing.T, before, after bridgestore.OAuthToken, wantClass RefreshErrorClass, wantPermanent bool) {
	t.Helper()
	if !bytes.Equal(after.EncryptedPayload, before.EncryptedPayload) || after.UpdatedAt != before.UpdatedAt || after.LastRefreshAt != before.LastRefreshAt {
		t.Fatalf(
			"token material or timestamps changed: payload_equal=%t before_updated=%d after_updated=%d before_refresh=%d after_refresh=%d",
			bytes.Equal(after.EncryptedPayload, before.EncryptedPayload),
			before.UpdatedAt, after.UpdatedAt, before.LastRefreshAt, after.LastRefreshAt,
		)
	}
	if after.LastRefreshErrorClass != string(wantClass) || after.ReauthRequired != wantPermanent {
		t.Fatalf("failure state = class %q reauth %t, want %q/%t", after.LastRefreshErrorClass, after.ReauthRequired, wantClass, wantPermanent)
	}
}

func assertClientAssertion(t *testing.T, token string, key *ecdsa.PublicKey, clientID, audience string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("client assertion has %d parts", len(parts))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if len(signature) != 64 {
		t.Fatalf("signature length = %d, want 64", len(signature))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		t.Fatal("client assertion signature is invalid")
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != clientID || claims["sub"] != clientID || claims["aud"] != audience {
		t.Fatalf("client assertion claims = %#v", claims)
	}
	if _, ok := claims["jti"].(string); !ok {
		t.Fatalf("client assertion jti = %#v", claims["jti"])
	}
}

func jwtClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestHandleCallbackRejectsUnknownOrExpiredState(t *testing.T) {
	client, stateStore := newTestClient(t, "https://issuer.example")
	for _, test := range []struct{ name, state string }{{"unknown", "missing"}, {"expired", "expired"}} {
		t.Run(test.name, func(t *testing.T) {
			if test.state == "expired" {
				if err := stateStore.SaveOAuthSession(context.Background(), oauthTestScope, bridgestore.OAuthSession{State: test.state, EncryptedPayload: []byte("not-a-valid-payload"), ExpiresAt: time.Now().Add(-time.Minute).Unix()}); err != nil {
					t.Fatal(err)
				}
			}
			recorder := httptest.NewRecorder()
			client.HandleCallback(recorder, httptest.NewRequest(http.MethodGet, "/oauth/callback?state="+test.state+"&code=code", nil))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", recorder.Code)
			}
		})
	}
}

func TestHandleCallbackRejectsMismatchedIssuer(t *testing.T) {
	var state string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validMetadataBody(server.URL)))
			return
		}
		if r.URL.Path != "/oauth/par" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		state = r.Form.Get("state")
		w.Header().Set("DPoP-Nonce", "nonce")
		_ = json.NewEncoder(w).Encode(map[string]string{"request_uri": "urn:test"})
	}))
	defer server.Close()
	client, _ := newTestClient(t, server.URL, server.Client())
	if _, err := client.StartAuthorization(context.Background(), "alice.test"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "&iss=https%3A%2F%2Fevil.example"} {
		recorder := httptest.NewRecorder()
		client.HandleCallback(recorder, httptest.NewRequest(http.MethodGet, "/oauth/callback?state="+url.QueryEscape(state)+"&code=code"+query, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("query %q: status = %d, want 400", query, recorder.Code)
		}
	}
}

func newTestClient(t *testing.T, issuer string, httpClients ...*http.Client) (*Client, bridgestore.OAuthStore) {
	t.Helper()
	store, closer, err := bridgestore.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encryptionKey := sha256.Sum256([]byte("test encryption key"))
	httpClient := http.DefaultClient
	if len(httpClients) > 0 {
		httpClient = httpClients[0]
	}
	client, err := NewClient(Options{Scope: oauthTestScope, Store: store, HTTPClient: httpClient, AuthorizationServerURL: issuer, ClientID: "https://bridge.example/oauth/client-metadata.json", RedirectURL: "https://bridge.example/oauth/callback", ClientSigningKey: key, EncryptionKey: encryptionKey[:]})
	if err != nil {
		t.Fatal(err)
	}
	return client, store
}

func TestTokenByAccountDIDSerializesConcurrentRefresh(t *testing.T) {
	fixedNow := time.Unix(2_000_000_000, 0)
	var refreshCalls int32
	refreshStarted := make(chan struct{})
	refreshGate := make(chan struct{})

	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validMetadataBody(server.URL)))
			return
		}
		if r.URL.Path != "/oauth/token" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") == "refresh_token" {
			count := atomic.AddInt32(&refreshCalls, 1)
			if count == 1 {
				close(refreshStarted)
			}
			<-refreshGate
			w.Header().Set("DPoP-Nonce", "refreshed-nonce")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "new-access",
				"refresh_token": "new-refresh",
				"scope":         "atproto",
				"expires_in":    3600,
			})
			return
		}
		t.Fatalf("unexpected grant_type %s", r.Form.Get("grant_type"))
	}))
	defer server.Close()

	client, store := newTestClient(t, server.URL, server.Client())
	client.now = func() time.Time { return fixedNow }
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(tokenPayload{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		Scope:        "atproto",
		DPoPKey:      privateJWK(key),
		Expiry:       fixedNow.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := client.encrypt(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveOAuthToken(context.Background(), oauthTestScope, bridgestore.OAuthToken{
		AccountDID:       "did:plc:alice",
		EncryptedPayload: encrypted,
		UpdatedAt:        fixedNow.Add(-31 * 24 * time.Hour).Unix(),
		LastRefreshAt:    fixedNow.Add(-31 * 24 * time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	recordingStore := &recordingOAuthStore{OAuthStore: store}
	client.store = recordingStore

	const numAPICallers = 5
	const numCallers = numAPICallers + 1
	errCh := make(chan error, numCallers)
	tokens := make([]Token, numAPICallers)
	var maintenanceResult RefreshResult
	ready := make(chan struct{}, numCallers)
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < numAPICallers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			tok, err := client.TokenByAccountDID(context.Background(), "did:plc:alice")
			if err != nil {
				errCh <- err
				return
			}
			tokens[idx] = tok
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ready <- struct{}{}
		<-start
		result, err := client.RefreshIfDue(context.Background(), "did:plc:alice", 30*24*time.Hour)
		if err != nil {
			errCh <- err
			return
		}
		maintenanceResult = result
	}()

	for i := 0; i < numCallers; i++ {
		<-ready
	}
	close(start)
	<-refreshStarted
	close(refreshGate)

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("unexpected error from caller: %v", err)
		}
	}

	if got := atomic.LoadInt32(&refreshCalls); got != 1 {
		t.Fatalf("refresh endpoint calls = %d, want 1", got)
	}
	if recordingStore.saveCalls != 1 {
		t.Fatalf("SaveOAuthToken calls = %d, want 1", recordingStore.saveCalls)
	}

	for i, tok := range tokens {
		if tok.AccessToken != "new-access" || tok.RefreshToken != "new-refresh" {
			t.Fatalf("caller %d token = %#v, want new-access / new-refresh", i, tok)
		}
	}
	if maintenanceResult.Reason != RefreshReasonMaintenance {
		t.Fatalf("maintenance result = %#v", maintenanceResult)
	}
	if maintenanceResult.Refreshed && maintenanceResult.Token.AccessToken != "new-access" {
		t.Fatalf("maintenance refreshed token = %#v", maintenanceResult.Token)
	}

	storedToken, err := store.OAuthTokenByAccountDID(context.Background(), oauthTestScope, "did:plc:alice")
	if err != nil {
		t.Fatalf("failed to load stored token: %v", err)
	}
	var storedPayload tokenPayload
	if err := client.decryptJSON(storedToken.EncryptedPayload, &storedPayload); err != nil {
		t.Fatalf("failed to decrypt stored token: %v", err)
	}
	if storedPayload.AccessToken != "new-access" || storedPayload.RefreshToken != "new-refresh" {
		t.Fatalf("stored payload = %#v, want new-access / new-refresh", storedPayload)
	}
	if storedToken.LastRefreshAt != fixedNow.Unix() || storedToken.UpdatedAt != fixedNow.Unix() {
		t.Fatalf("stored timestamps = updated %d refresh %d, want %d", storedToken.UpdatedAt, storedToken.LastRefreshAt, fixedNow.Unix())
	}
}

func TestRefreshIfDueSkipsTokenRefreshed29DaysAgo(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("RefreshIfDue made an HTTP request before the refresh period elapsed")
		return nil, errors.New("unexpected HTTP request")
	})}
	client, store := newTestClient(t, "https://issuer.example", httpClient)
	client.now = func() time.Time { return now }
	saveStatusTestToken(t, client, store, tokenPayload{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		Scope:        "atproto",
		DPoPKey:      newStatusTestJWK(t),
		Expiry:       now.Add(time.Hour),
	}, bridgestore.OAuthToken{
		AccountDID:    "did:plc:alice",
		LastRefreshAt: now.Add(-29 * 24 * time.Hour).Unix(),
	})

	result, err := client.RefreshIfDue(context.Background(), "did:plc:alice", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refreshed || result.Reason != RefreshReasonMaintenance {
		t.Fatalf("result = %#v", result)
	}
}

func TestRefreshIfDueRejectsDifferentConfiguredAccountBeforeStoreOrHTTP(t *testing.T) {
	var httpCalls int32
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&httpCalls, 1)
		return nil, errors.New("unexpected HTTP request")
	})}
	client, store := newTestClient(t, "https://issuer.example", httpClient)
	client.store = failRefreshStore{OAuthStore: store, t: t}

	result, err := client.RefreshIfDue(context.Background(), "did:plc:mallory", 30*24*time.Hour)
	if !errors.Is(err, bridgestore.ErrSourceScopeMismatch) {
		t.Fatalf("error = %v, want source scope mismatch", err)
	}
	if result.Refreshed || result.Reason != RefreshReasonMaintenance {
		t.Fatalf("result = %#v", result)
	}
	if got := atomic.LoadInt32(&httpCalls); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0", got)
	}
}

type failRefreshStore struct {
	bridgestore.OAuthStore
	t *testing.T
}

func (s failRefreshStore) OAuthTokenByAccountDID(context.Context, bridgestore.SourceScope, string) (bridgestore.OAuthToken, error) {
	s.t.Fatal("RefreshIfDue loaded an OAuth token for a different configured account")
	return bridgestore.OAuthToken{}, errors.New("unexpected persistence access")
}

func (s failRefreshStore) SaveOAuthToken(context.Context, bridgestore.SourceScope, bridgestore.OAuthToken) error {
	s.t.Fatal("RefreshIfDue saved an OAuth token for a different configured account")
	return errors.New("unexpected persistence mutation")
}

func (s failRefreshStore) UpdateOAuthTokenRefreshFailure(context.Context, bridgestore.SourceScope, string, string, bool) error {
	s.t.Fatal("RefreshIfDue updated refresh failure state for a different configured account")
	return errors.New("unexpected persistence mutation")
}

func TestRefreshIfDueUsesLegacyUpdatedAtAt30DayBoundaryAfterClientReload(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	var tokenRequests int32
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validMetadataBody(server.URL)))
		case "/oauth/token":
			atomic.AddInt32(&tokenRequests, 1)
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if got := r.Form.Get("grant_type"); got != "refresh_token" {
				t.Fatalf("grant_type = %q", got)
			}
			w.Header().Set("DPoP-Nonce", "maintenance-nonce")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "refreshed-access",
				"refresh_token": "refreshed-refresh",
				"scope":         "atproto",
				"expires_in":    3600,
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client, store := newTestClient(t, server.URL, server.Client())
	saveStatusTestToken(t, client, store, tokenPayload{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		Scope:        "atproto",
		DPoPKey:      newStatusTestJWK(t),
		Expiry:       now.Add(time.Hour),
	}, bridgestore.OAuthToken{
		AccountDID:            "did:plc:alice",
		UpdatedAt:             now.Add(-30 * 24 * time.Hour).Unix(),
		LastRefreshErrorClass: string(RefreshErrorRateLimit),
	})
	recordingStore := &recordingOAuthStore{OAuthStore: store}
	encryptionKey := sha256.Sum256([]byte("test encryption key"))
	reloaded, err := NewClient(Options{
		Scope:                  oauthTestScope,
		Store:                  recordingStore,
		HTTPClient:             server.Client(),
		AuthorizationServerURL: server.URL,
		ClientID:               client.clientID,
		RedirectURL:            client.redirectURL,
		ClientSigningKey:       client.clientSigningKey,
		EncryptionKey:          encryptionKey[:],
		Now:                    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := reloaded.RefreshIfDue(context.Background(), "did:plc:alice", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Refreshed || result.Reason != RefreshReasonMaintenance || result.Token.AccessToken != "refreshed-access" {
		t.Fatalf("result = %#v", result)
	}
	if got := atomic.LoadInt32(&tokenRequests); got != 1 {
		t.Fatalf("token endpoint requests = %d, want 1", got)
	}
	if recordingStore.saveCalls != 1 {
		t.Fatalf("SaveOAuthToken calls = %d, want 1", recordingStore.saveCalls)
	}
	stored, err := store.OAuthTokenByAccountDID(context.Background(), oauthTestScope, "did:plc:alice")
	if err != nil {
		t.Fatal(err)
	}
	if stored.UpdatedAt != now.Unix() || stored.LastRefreshAt != now.Unix() || stored.ReauthRequired || stored.LastRefreshErrorClass != "" {
		t.Fatalf(
			"stored refresh state = updated %d refresh %d reauth %t class %q",
			stored.UpdatedAt, stored.LastRefreshAt, stored.ReauthRequired, stored.LastRefreshErrorClass,
		)
	}
	var storedPayload tokenPayload
	if err := reloaded.decryptJSON(stored.EncryptedPayload, &storedPayload); err != nil {
		t.Fatal(err)
	}
	if storedPayload.AccessToken != "refreshed-access" ||
		storedPayload.RefreshToken != "refreshed-refresh" ||
		storedPayload.DPoPNonce != "maintenance-nonce" ||
		!storedPayload.Expiry.Equal(now.Add(time.Hour)) {
		t.Fatalf("stored maintenance payload = %#v", storedPayload)
	}
}
