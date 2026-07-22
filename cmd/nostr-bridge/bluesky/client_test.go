package bluesky

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	bridgeoauth "github.com/nakatanakatana/mytools/cmd/nostr-bridge/oauth"
)

func TestClientFetchesAuthenticatedPagedSources(t *testing.T) {
	requests := make([]string, 0, 5)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "DPoP access-token" {
			t.Fatal("unexpected Authorization header")
		}
		assertDPoPProof(t, r, key, "persisted-nonce", "access-token")
		requests = append(requests, r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case "/xrpc/app.bsky.feed.getTimeline":
			_ = json.NewEncoder(w).Encode(map[string]any{"cursor": "next", "feed": []any{
				map[string]any{"post": map[string]any{
					"uri": "at://did:plc:one/app.bsky.feed.post/1", "cid": "cid", "author": map[string]any{"did": "did:plc:one"},
					"embed": map[string]any{"$type": "app.bsky.embed.images#view", "images": []any{map[string]any{
						"fullsize": "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:one/image@jpeg", "alt": "A description", "aspectRatio": map[string]any{"width": 1200, "height": 800},
					}}},
				}},
				map[string]any{"post": map[string]any{
					"uri": "at://did:plc:one/app.bsky.feed.post/2", "cid": "cid-2", "author": map[string]any{"did": "did:plc:one"},
					"record": map[string]any{
						"facets": []any{map[string]any{
							"features": []any{map[string]any{"$type": "app.bsky.richtext.facet#link", "uri": "https://facet.example/path"}},
						}},
					},
					"embed": map[string]any{"$type": "app.bsky.embed.external#view", "external": map[string]any{"uri": "https://embed.example/path"}},
				}},
			}})
		case "/xrpc/app.bsky.graph.getFollows":
			if r.URL.Query().Get("actor") != "did:plc:owner" {
				t.Fatalf("actor = %q", r.URL.Query().Get("actor"))
			}
			if r.URL.Query().Get("cursor") == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"cursor": "page-two", "follows": []any{map[string]any{"did": "did:plc:one"}}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"follows": []any{map[string]any{"did": "did:plc:two"}}})
		case "/xrpc/app.bsky.graph.getList":
			if r.URL.Query().Get("list") != "at://did:plc:owner/app.bsky.graph.list/one" {
				t.Fatalf("list = %q", r.URL.Query().Get("list"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"list": map[string]any{"uri": "at://did:plc:owner/app.bsky.graph.list/one", "name": "Friends", "description": "People I know"}, "items": []any{map[string]any{"subject": map[string]any{"did": "did:plc:three"}}}})
		case "/xrpc/app.bsky.actor.getProfile":
			_ = json.NewEncoder(w).Encode(map[string]any{"did": "did:plc:one", "handle": "one.test", "displayName": "One", "description": "bio", "avatar": "https://cdn.example/one.jpg"})
		default:
			t.Fatalf("unexpected request %s", r.URL)
		}
	}))
	defer server.Close()

	client, err := NewClient(ClientOptions{HTTPClient: server.Client(), BaseURL: server.URL, Token: bridgeoauth.Token{AccessToken: "access-token", DPoPKey: key, DPoPNonce: "persisted-nonce"}, AccountDID: "did:plc:owner"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.Timeline(context.Background(), "", 25)
	if err != nil || page.Cursor != "next" || len(page.Posts) != 2 || page.Posts[0].AuthorDID != "did:plc:one" {
		t.Fatalf("Timeline = %#v, %v", page, err)
	}
	if images := page.Posts[0].Images; len(images) != 1 || images[0].URL != "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:one/image@jpeg" || images[0].MIMEType != "image/jpeg" || images[0].Alt != "A description" || images[0].Width != 1200 || images[0].Height != 800 {
		t.Fatalf("Timeline images = %#v", images)
	}
	if links := page.Posts[1].Links; len(links) != 2 || links[0].URI != "https://facet.example/path" || links[1].URI != "https://embed.example/path" {
		t.Fatalf("Timeline links = %#v", links)
	}
	follows, err := client.Follows(context.Background())
	if err != nil || len(follows) != 2 || follows[0].DID != "did:plc:one" || follows[1].DID != "did:plc:two" {
		t.Fatalf("Follows = %#v, %v", follows, err)
	}
	list, err := client.List(context.Background(), "at://did:plc:owner/app.bsky.graph.list/one")
	if err != nil || list.Name != "Friends" || list.Description != "People I know" || len(list.Members) != 1 || list.Members[0].DID != "did:plc:three" {
		t.Fatalf("List = %#v, %v", list, err)
	}
	profile, err := client.Profile(context.Background(), "did:plc:one")
	if err != nil || profile.Handle != "one.test" || profile.Avatar != "https://cdn.example/one.jpg" {
		t.Fatalf("Profile = %#v, %v", profile, err)
	}
	if len(requests) != 5 {
		t.Fatalf("requests = %#v, want five", requests)
	}
}

func TestClientRetriesDPoPNonceChallenge(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("DPoP-Nonce", "challenge-nonce")
			http.Error(w, "use DPoP nonce", http.StatusUnauthorized)
			return
		}
		assertDPoPProof(t, r, key, "challenge-nonce", "access-token")
		_ = json.NewEncoder(w).Encode(map[string]any{"feed": []any{}})
	}))
	defer server.Close()
	client, err := NewClient(ClientOptions{HTTPClient: server.Client(), BaseURL: server.URL, Token: bridgeoauth.Token{AccessToken: "access-token", DPoPKey: key}, AccountDID: "did:plc:owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Timeline(context.Background(), "", 1); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestClientUsesDPoPNonceFromSuccessfulResponseOnNextRequest(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		switch attempts {
		case 1:
			assertDPoPProof(t, r, key, "", "access-token")
			w.Header().Set("DPoP-Nonce", "success-response-nonce")
			_ = json.NewEncoder(w).Encode(map[string]any{"feed": []any{}})
		case 2:
			assertDPoPProof(t, r, key, "success-response-nonce", "access-token")
			_ = json.NewEncoder(w).Encode(map[string]any{"did": "did:plc:one", "handle": "one.test"})
		default:
			t.Fatal("unexpected additional PDS request")
		}
	}))
	defer server.Close()
	client, err := NewClient(ClientOptions{HTTPClient: server.Client(), BaseURL: server.URL, Token: bridgeoauth.Token{AccessToken: "access-token", DPoPKey: key}, AccountDID: "did:plc:owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Timeline(context.Background(), "", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Profile(context.Background(), "did:plc:one"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestClientReportsSafePDSFailure(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var proof string
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		proof = r.Header.Get("DPoP")
		w.Header().Set("DPoP-Nonce", "secret-nonce")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "UseDpopNonce", "message": "proof rejected"})
	}))
	defer server.Close()
	client, err := NewClient(ClientOptions{HTTPClient: server.Client(), BaseURL: server.URL, Token: bridgeoauth.Token{AccessToken: "access-token", DPoPKey: key}, AccountDID: "did:plc:owner"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Timeline(context.Background(), "", 1)
	if err == nil {
		t.Fatal("Timeline error = nil, want PDS failure")
	}
	errorText := err.Error()
	for _, want := range []string{"app.bsky.feed.getTimeline", "401 Unauthorized", "UseDpopNonce", "proof rejected", "DPoP-Nonce header present=true"} {
		if !strings.Contains(errorText, want) {
			t.Errorf("Timeline error missing %q", want)
		}
	}
	for _, secret := range []string{"secret-nonce", "access-token", proof} {
		if secret != "" && strings.Contains(errorText, secret) {
			t.Errorf("Timeline error contains secret data")
		}
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func assertDPoPProof(t *testing.T, request *http.Request, wantKey *ecdsa.PrivateKey, wantNonce, accessToken string) {
	t.Helper()
	parts := strings.Split(request.Header.Get("DPoP"), ".")
	if len(parts) != 3 {
		t.Fatal("invalid DPoP proof structure")
	}
	var header struct {
		Typ string `json:"typ"`
		Alg string `json:"alg"`
		JWK struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"jwk"`
	}
	var claims struct {
		HTM   string `json:"htm"`
		HTU   string `json:"htu"`
		Nonce string `json:"nonce"`
		JTI   string `json:"jti"`
		ATH   string `json:"ath"`
	}
	decodeJWTPart(t, parts[0], &header)
	decodeJWTPart(t, parts[1], &claims)
	if header.Typ != "dpop+jwt" || header.Alg != "ES256" || header.JWK.Kty != "EC" || header.JWK.Crv != "P-256" {
		t.Fatalf("DPoP header = %#v", header)
	}
	xBytes := make([]byte, 32)
	yBytes := make([]byte, 32)
	wantKey.X.FillBytes(xBytes)
	wantKey.Y.FillBytes(yBytes)
	if header.JWK.X != base64.RawURLEncoding.EncodeToString(xBytes) || header.JWK.Y != base64.RawURLEncoding.EncodeToString(yBytes) {
		t.Fatalf("DPoP JWK does not match persisted key: %#v", header.JWK)
	}
	if claims.HTM != request.Method || claims.HTU != "http://"+request.Host+request.URL.Path || claims.Nonce != wantNonce || claims.JTI == "" {
		t.Fatalf("unexpected DPoP claims for request %s", request.URL)
	}
	ath := sha256.Sum256([]byte(accessToken))
	if claims.ATH != base64.RawURLEncoding.EncodeToString(ath[:]) {
		t.Fatal("unexpected DPoP ath claim")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		t.Fatalf("decode DPoP signature: %v, %d bytes", err, len(signature))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&wantKey.PublicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		t.Fatal("DPoP signature does not verify")
	}
}

func decodeJWTPart(t *testing.T, part string, destination any) {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(decoded, destination); err != nil {
		t.Fatal(err)
	}
}

type sequenceTokenProvider struct {
	tokens []bridgeoauth.Token
	calls  []string
}

func (p *sequenceTokenProvider) TokenByAccountDID(ctx context.Context, did string) (bridgeoauth.Token, error) {
	p.calls = append(p.calls, did)
	if len(p.tokens) == 0 {
		return bridgeoauth.Token{}, errors.New("no token available")
	}
	tok := p.tokens[0]
	p.tokens = p.tokens[1:]
	return tok, nil
}

func TestClientLoadsTokenForEachRequest(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			if r.Header.Get("Authorization") != "DPoP first-access" {
				t.Fatalf("request 1 Authorization = %q, want DPoP first-access", r.Header.Get("Authorization"))
			}
			assertDPoPProof(t, r, key, "", "first-access")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"did": "did:plc:one", "handle": "one.test",
			})
		case 2:
			if r.Header.Get("Authorization") != "DPoP second-access" {
				t.Fatalf("request 2 Authorization = %q, want DPoP second-access", r.Header.Get("Authorization"))
			}
			assertDPoPProof(t, r, key, "", "second-access")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"did": "did:plc:two", "handle": "two.test",
			})
		default:
			t.Fatalf("unexpected request %d", requestCount)
		}
	}))
	defer server.Close()

	provider := &sequenceTokenProvider{
		tokens: []bridgeoauth.Token{
			{AccessToken: "first-access", DPoPKey: key},
			{AccessToken: "second-access", DPoPKey: key},
		},
	}

	client, err := NewClient(ClientOptions{
		HTTPClient: server.Client(),
		BaseURL:    server.URL,
		Tokens:     provider,
		AccountDID: "did:plc:owner",
	})
	if err != nil {
		t.Fatal(err)
	}

	p1, err := client.Profile(context.Background(), "did:plc:one")
	if err != nil || p1.Handle != "one.test" {
		t.Fatalf("Profile 1 = %#v, %v", p1, err)
	}

	p2, err := client.Profile(context.Background(), "did:plc:two")
	if err != nil || p2.Handle != "two.test" {
		t.Fatalf("Profile 2 = %#v, %v", p2, err)
	}

	if len(provider.calls) != 2 {
		t.Fatalf("provider calls count = %d, want 2", len(provider.calls))
	}
	if provider.calls[0] != "did:plc:owner" || provider.calls[1] != "did:plc:owner" {
		t.Fatalf("provider calls = %#v, want [did:plc:owner, did:plc:owner]", provider.calls)
	}
}

type errorTokenProvider struct {
	err error
}

func (p *errorTokenProvider) TokenByAccountDID(ctx context.Context, did string) (bridgeoauth.Token, error) {
	return bridgeoauth.Token{}, p.err
}

func TestClientDoesNotRequestWithTokenProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected HTTP request when token provider returns error")
	}))
	defer server.Close()

	provider := &errorTokenProvider{err: errors.New("token store error")}

	client, err := NewClient(ClientOptions{
		HTTPClient: server.Client(),
		BaseURL:    server.URL,
		Tokens:     provider,
		AccountDID: "did:plc:owner",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Profile(context.Background(), "did:plc:owner")
	if err == nil {
		t.Fatal("Profile err = nil, want token store error")
	}
	if !strings.Contains(err.Error(), "token store error") {
		t.Fatalf("Profile err = %v, want token store error contained", err)
	}
}

func TestClientConcurrentGetDPoPNonceRace(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tok := bridgeoauth.Token{
		AccessToken: "test-token",
		DPoPKey:     key,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("DPoP-Nonce", "new-nonce")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"did":    "did:plc:owner",
			"handle": "owner.test",
		})
	}))
	defer server.Close()

	client, err := NewClient(ClientOptions{
		HTTPClient: server.Client(),
		BaseURL:    server.URL,
		Token:      tok,
		AccountDID: "did:plc:owner",
	})
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 10
	errCh := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.Profile(context.Background(), "did:plc:owner")
			if err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent Profile failed: %v", err)
		}
	}
}

type invalidTokenProvider struct{}

func (p invalidTokenProvider) TokenByAccountDID(ctx context.Context, did string) (bridgeoauth.Token, error) {
	return bridgeoauth.Token{AccessToken: "", DPoPKey: nil}, nil
}

func TestClientDoesNotPanicWithInvalidToken(t *testing.T) {
	client, err := NewClient(ClientOptions{
		HTTPClient: http.DefaultClient,
		BaseURL:    "http://127.0.0.1",
		Tokens:     invalidTokenProvider{},
		AccountDID: "did:plc:owner",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Profile(context.Background(), "did:plc:owner")
	if err == nil {
		t.Fatal("Profile err = nil, want invalid token error")
	}
	if !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("Profile err = %v, want 'invalid token' error", err)
	}
}

func TestClientPreventsStaleDPoPNonceOverwrite(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tok := bridgeoauth.Token{
		AccessToken: "test-token",
		DPoPKey:     key,
	}
	client, err := NewClient(ClientOptions{
		HTTPClient: http.DefaultClient,
		BaseURL:    "http://127.0.0.1",
		Token:      tok,
		AccountDID: "did:plc:owner",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, gen0 := client.nonceAndGen()

	// Update with gen0 succeeds and advances generation
	if !client.updateNonce(gen0, "nonce-gen1") {
		t.Fatal("updateNonce with initial gen0 failed")
	}

	nonce, gen1 := client.nonceAndGen()
	if nonce != "nonce-gen1" || gen1 != gen0+1 {
		t.Fatalf("nonce = %q, gen = %d, want nonce-gen1, gen = %d", nonce, gen1, gen0+1)
	}

	// Attempt stale update with old gen0 fails
	if client.updateNonce(gen0, "stale-nonce-gen0") {
		t.Fatal("updateNonce with stale gen0 returned true, want false")
	}
	if nonce, _ := client.nonceAndGen(); nonce != "nonce-gen1" {
		t.Fatalf("nonce = %q, want nonce-gen1", nonce)
	}

	// Valid update with current gen1 succeeds
	if !client.updateNonce(gen1, "nonce-gen2") {
		t.Fatal("updateNonce with gen1 failed")
	}
	if nonce, _ := client.nonceAndGen(); nonce != "nonce-gen2" {
		t.Fatalf("nonce = %q, want nonce-gen2", nonce)
	}
}

func TestClientPreventsStaleDPoPNonceOverwriteConcurrentHTTP(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tok := bridgeoauth.Token{
		AccessToken: "test-token",
		DPoPKey:     key,
	}

	req1Started := make(chan struct{})
	req1Gate := make(chan struct{})
	var requestCount int32
	var gateOnce sync.Once
	closeGate := func() {
		gateOnce.Do(func() { close(req1Gate) })
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requestCount, 1)
		if count == 1 {
			close(req1Started)
			<-req1Gate
			w.Header().Set("DPoP-Nonce", "stale-slow-nonce")
		} else {
			w.Header().Set("DPoP-Nonce", "fresh-fast-nonce")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"did":    "did:plc:owner",
			"handle": "owner.test",
		})
	}))
	defer server.Close()
	defer closeGate()

	client, err := NewClient(ClientOptions{
		HTTPClient: server.Client(),
		BaseURL:    server.URL,
		Token:      tok,
		AccountDID: "did:plc:owner",
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	// Req 1 (slow)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = client.Profile(context.Background(), "did:plc:owner")
	}()

	<-req1Started

	// Req 2 (fast, completes first and updates nonce to fresh-fast-nonce)
	_, err = client.Profile(context.Background(), "did:plc:owner")
	if err != nil {
		t.Fatalf("fast Profile failed: %v", err)
	}

	// Release slow Req 1
	closeGate()
	wg.Wait()

	// Verify that the slow response did not overwrite fresh-fast-nonce with stale-slow-nonce
	nonce, _ := client.nonceAndGen()
	if nonce != "fresh-fast-nonce" {
		t.Fatalf("dpopNonce = %q, want fresh-fast-nonce", nonce)
	}
}

func parseDPoPNonce(t *testing.T, proofJWT string) string {
	t.Helper()
	parts := strings.Split(proofJWT, ".")
	if len(parts) != 3 {
		t.Fatalf("invalid DPoP JWT format: %s", proofJWT)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("failed to decode DPoP payload: %v", err)
	}
	var claims struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("failed to unmarshal DPoP claims: %v", err)
	}
	return claims.Nonce
}

func TestClientDPoPChallengeRetryStaleAndFailureNonce(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tok := bridgeoauth.Token{
		AccessToken: "test-token",
		DPoPKey:     key,
	}

	slowReqStarted := make(chan struct{})
	slowReqGate := make(chan struct{})
	var gateOnce sync.Once
	closeGate := func() {
		gateOnce.Do(func() { close(slowReqGate) })
	}

	var reqCount int32
	var retriedProofNonce string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&reqCount, 1)
		switch count {
		case 1:
			// Slow req 1: receives 401 challenge after gate
			close(slowReqStarted)
			<-slowReqGate
			w.Header().Set("DPoP-Nonce", "stale-401-challenge")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "use_dpop_nonce"})
		case 2:
			// Fast req 2: completes first and saves fast-fresh-nonce
			w.Header().Set("DPoP-Nonce", "fast-fresh-nonce")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"did": "did:plc:owner", "handle": "owner.test"})
		case 3:
			// Slow req 1 retry: verify proof nonce and return final failure with new nonce
			retriedProofNonce = r.Header.Get("DPoP")
			w.Header().Set("DPoP-Nonce", "final-failure-nonce")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_request"})
		}
	}))
	defer server.Close()
	defer closeGate()

	client, err := NewClient(ClientOptions{
		HTTPClient: server.Client(),
		BaseURL:    server.URL,
		Token:      tok,
		AccountDID: "did:plc:owner",
	})
	if err != nil {
		t.Fatal(err)
	}

	var slowErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, slowErr = client.Profile(context.Background(), "did:plc:owner")
	}()

	<-slowReqStarted

	// Fast request completes and updates nonce to fast-fresh-nonce
	_, err = client.Profile(context.Background(), "did:plc:owner")
	if err != nil {
		t.Fatalf("fast Profile failed: %v", err)
	}

	// Release slow request to receive 401 challenge and retry
	closeGate()
	wg.Wait()

	// 1. Verify slow request returned expected error containing both invalid_request and 400 Bad Request status
	if slowErr == nil || !strings.Contains(slowErr.Error(), "invalid_request") || !strings.Contains(slowErr.Error(), "400 Bad Request") {
		t.Fatalf("slow Profile err = %v, want invalid_request and 400 Bad Request contained", slowErr)
	}

	// 2. Verify total request count was exactly 3 (no extra retries)
	if count := atomic.LoadInt32(&reqCount); count != 3 {
		t.Fatalf("request count = %d, want 3", count)
	}

	// 3. Verify retried proof used fast-fresh-nonce (the latest nonce available when slow request retried)
	usedNonce := parseDPoPNonce(t, retriedProofNonce)
	if usedNonce != "fast-fresh-nonce" {
		t.Fatalf("retried DPoP proof nonce = %q, want fast-fresh-nonce", usedNonce)
	}

	// 4. Verify that the final failure response updated the nonce to final-failure-nonce
	nonce, _ := client.nonceAndGen()
	if nonce != "final-failure-nonce" {
		t.Fatalf("dpopNonce = %q, want final-failure-nonce", nonce)
	}
}
