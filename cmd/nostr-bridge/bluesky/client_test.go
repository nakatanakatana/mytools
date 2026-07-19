package bluesky

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
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
		assertDPoPProof(t, r, key, "", "access-token")
		requests = append(requests, r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case "/xrpc/app.bsky.feed.getTimeline":
			_ = json.NewEncoder(w).Encode(map[string]any{"cursor": "next", "feed": []any{map[string]any{"post": map[string]any{
				"uri": "at://did:plc:one/app.bsky.feed.post/1", "cid": "cid", "author": map[string]any{"did": "did:plc:one"},
				"embed": map[string]any{"$type": "app.bsky.embed.images#view", "images": []any{map[string]any{
					"fullsize": "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:one/image@jpeg", "alt": "A description", "aspectRatio": map[string]any{"width": 1200, "height": 800},
				}}},
			}}}})
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
	if err != nil || page.Cursor != "next" || len(page.Posts) != 1 || page.Posts[0].AuthorDID != "did:plc:one" {
		t.Fatalf("Timeline = %#v, %v", page, err)
	}
	if images := page.Posts[0].Images; len(images) != 1 || images[0].URL != "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:one/image@jpeg" || images[0].MIMEType != "image/jpeg" || images[0].Alt != "A description" || images[0].Width != 1200 || images[0].Height != 800 {
		t.Fatalf("Timeline images = %#v", images)
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
	if header.JWK.X != base64.RawURLEncoding.EncodeToString(wantKey.X.Bytes()) || header.JWK.Y != base64.RawURLEncoding.EncodeToString(wantKey.Y.Bytes()) {
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
