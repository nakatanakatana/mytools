package oauth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

var oauthTestScope = bridgestore.SourceScope{}

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

func TestHandleCallbackExchangesCodeAndSavesEncryptedTokens(t *testing.T) {
	var tokenForm url.Values
	var tokenDPoPs []string
	var state string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
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
	if _, err := client.StartAuthorization(context.Background(), "alice.test"); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("token payload was not encrypted: %q", token.EncryptedPayload)
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
}

func TestTokenByAccountDIDRefreshesExpiredTokenAndPersistsRotation(t *testing.T) {
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
	payload, err := json.Marshal(tokenPayload{AccessToken: "old-access", RefreshToken: "old-refresh", Scope: "atproto", DPoPKey: privateJWK(key), Expiry: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := client.encrypt(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveOAuthToken(context.Background(), oauthTestScope, bridgestore.OAuthToken{AccountDID: "did:plc:alice", EncryptedPayload: encrypted}); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewClient(Options{Store: store, HTTPClient: server.Client(), AuthorizationServerURL: server.URL, ClientID: client.clientID, RedirectURL: client.redirectURL, ClientSigningKey: client.clientSigningKey, EncryptionKey: client.encryptionKey})
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
	client, err := NewClient(Options{Store: store, HTTPClient: httpClient, AuthorizationServerURL: issuer, ClientID: "https://bridge.example/oauth/client-metadata.json", RedirectURL: "https://bridge.example/oauth/callback", ClientSigningKey: key, EncryptionKey: encryptionKey[:]})
	if err != nil {
		t.Fatal(err)
	}
	return client, store
}
