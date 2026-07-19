// Package oauth implements the AT Protocol confidential-client OAuth flow.
package oauth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

const (
	clientAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	clientScope         = "atproto" +
		" rpc:app.bsky.graph.getFollows?aud=did:web:api.bsky.app%23bsky_appview" +
		" rpc:app.bsky.graph.getList?aud=did:web:api.bsky.app%23bsky_appview" +
		" rpc:app.bsky.actor.getProfile?aud=did:web:api.bsky.app%23bsky_appview" +
		" rpc:app.bsky.feed.getTimeline?aud=did:web:api.bsky.app%23bsky_appview"
)

// Options configures a confidential AT Protocol OAuth client.
type Options struct {
	Scope                  bridgestore.SourceScope
	Store                  bridgestore.OAuthStore
	HTTPClient             *http.Client
	AuthorizationServerURL string
	ClientID               string
	RedirectURL            string
	ClientSigningKey       *ecdsa.PrivateKey
	EncryptionKey          []byte
	Now                    func() time.Time
}

// Client starts and completes OAuth authorization flows.
type Client struct {
	scope            bridgestore.SourceScope
	store            bridgestore.OAuthStore
	httpClient       *http.Client
	issuer           string
	clientID         string
	redirectURL      string
	clientSigningKey *ecdsa.PrivateKey
	encryptionKey    []byte
	now              func() time.Time
}

type sessionPayload struct {
	CodeVerifier string `json:"code_verifier"`
	Issuer       string `json:"issuer"`
	DPoPKey      jwk    `json:"dpop_key"`
	DPoPNonce    string `json:"dpop_nonce"`
}

type tokenPayload struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Scope        string    `json:"scope"`
	DPoPKey      jwk       `json:"dpop_key"`
	DPoPNonce    string    `json:"dpop_nonce"`
	Expiry       time.Time `json:"expiry"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Sub          string `json:"sub"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
}

// Token is an access token together with the DPoP credential to which it is bound.
// Its private key is reconstructed from the encrypted token payload each time it
// is loaded, so callers can create proofs without separately persisting key data.
type Token struct {
	AccessToken  string
	RefreshToken string
	Scope        string
	DPoPKey      *ecdsa.PrivateKey
	DPoPNonce    string
	Expiry       time.Time
}

// NewClient returns a client configured with an encryption key and ES256 signing key.
func NewClient(options Options) (*Client, error) {
	if options.Store == nil || options.ClientSigningKey == nil || len(options.EncryptionKey) != 32 {
		return nil, errors.New("OAuth client requires store, ES256 signing key, and 32-byte encryption key")
	}
	if options.AuthorizationServerURL == "" || options.ClientID == "" || options.RedirectURL == "" {
		return nil, errors.New("OAuth client requires authorization server, client ID, and redirect URL")
	}
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Client{scope: options.Scope, store: options.Store, httpClient: options.HTTPClient, issuer: strings.TrimRight(options.AuthorizationServerURL, "/"), clientID: options.ClientID, redirectURL: options.RedirectURL, clientSigningKey: options.ClientSigningKey, encryptionKey: append([]byte(nil), options.EncryptionKey...), now: options.Now}, nil
}

// StartAuthorization creates a stateful PAR request and returns the user-facing authorization URL.
func (c *Client) StartAuthorization(ctx context.Context, handle string) (string, error) {
	metadata, err := c.discoverAuthorizationServer(ctx, c.issuer)
	if err != nil {
		return "", fmt.Errorf("discover authorization server metadata: %w", err)
	}
	state, err := randomString(32)
	if err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	verifier, err := randomString(48)
	if err != nil {
		return "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	dpopKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate DPoP key: %w", err)
	}
	dpopJWK := privateJWK(dpopKey)

	challengeHash := sha256.Sum256([]byte(verifier))
	form := url.Values{
		"client_id": {c.clientID}, "response_type": {"code"}, "redirect_uri": {c.redirectURL}, "scope": {clientScope},
		"state": {state}, "login_hint": {handle}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challengeHash[:])}, "code_challenge_method": {"S256"},
		"client_assertion_type": {clientAssertionType},
	}
	response, err := c.doDPoPFormRequest(ctx, "push authorization request", metadata.PushedAuthorizationRequestEndpoint, dpopKey, "", func() (url.Values, error) {
		attemptForm := cloneValues(form)
		assertion, err := c.clientAssertion(metadata.Issuer)
		if err != nil {
			return nil, err
		}
		attemptForm.Set("client_assertion", assertion)
		return attemptForm, nil
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return "", responseError("push authorization request", response)
	}
	var par struct {
		RequestURI string `json:"request_uri"`
		ExpiresIn  int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(response.Body).Decode(&par); err != nil {
		return "", fmt.Errorf("decode PAR response: %w", err)
	}
	if par.RequestURI == "" {
		return "", errors.New("PAR response did not include request_uri")
	}
	expiresIn := par.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	payload, err := json.Marshal(sessionPayload{CodeVerifier: verifier, Issuer: metadata.Issuer, DPoPKey: dpopJWK, DPoPNonce: response.Header.Get("DPoP-Nonce")})
	if err != nil {
		return "", fmt.Errorf("encode OAuth session: %w", err)
	}
	encrypted, err := c.encrypt(payload)
	if err != nil {
		return "", err
	}
	if err := c.store.SaveOAuthSession(ctx, c.scope, bridgestore.OAuthSession{State: state, EncryptedPayload: encrypted, ExpiresAt: c.now().Add(time.Duration(expiresIn) * time.Second).Unix()}); err != nil {
		return "", err
	}
	authorize, _ := url.Parse(metadata.AuthorizationEndpoint)
	authorize.RawQuery = url.Values{"client_id": {c.clientID}, "request_uri": {par.RequestURI}}.Encode()
	return authorize.String(), nil
}

// HandleCallback validates state, exchanges the code, encrypts the returned token payload, and redirects home.
func (c *Client) HandleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if state == "" || code == "" {
		http.Error(w, "invalid OAuth callback", http.StatusBadRequest)
		return
	}
	session, err := c.store.OAuthSessionByState(r.Context(), c.scope, state)
	if err != nil || session.ExpiresAt <= c.now().Unix() {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	var payload sessionPayload
	if err := c.decryptJSON(session.EncryptedPayload, &payload); err != nil {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	if issuer := r.URL.Query().Get("iss"); issuer == "" || issuer != payload.Issuer {
		http.Error(w, "invalid OAuth issuer", http.StatusBadRequest)
		return
	}
	metadata, err := c.discoverAuthorizationServer(r.Context(), payload.Issuer)
	if err != nil {
		http.Error(w, "OAuth token exchange failed", http.StatusBadGateway)
		return
	}
	dpopKey, err := payload.DPoPKey.ecdsa()
	if err != nil {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}

	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {c.redirectURL}, "client_id": {c.clientID}, "code_verifier": {payload.CodeVerifier}, "client_assertion_type": {clientAssertionType}}
	response, err := c.doDPoPFormRequest(r.Context(), "exchange OAuth code", metadata.TokenEndpoint, dpopKey, payload.DPoPNonce, func() (url.Values, error) {
		attemptForm := cloneValues(form)
		assertion, err := c.clientAssertion(metadata.Issuer)
		if err != nil {
			return nil, err
		}
		attemptForm.Set("client_assertion", assertion)
		return attemptForm, nil
	})
	if err != nil {
		http.Error(w, "OAuth token exchange failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		http.Error(w, "OAuth token exchange failed", http.StatusBadGateway)
		return
	}
	var tokens tokenResponse
	if err := json.NewDecoder(response.Body).Decode(&tokens); err != nil || tokens.Sub == "" || !hasScope(tokens.Scope, "atproto") {
		http.Error(w, "invalid OAuth token response", http.StatusBadGateway)
		return
	}
	var expiry time.Time
	if tokens.ExpiresIn > 0 {
		expiry = c.now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	}
	encoded, err := json.Marshal(tokenPayload{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, Scope: tokens.Scope, DPoPKey: payload.DPoPKey, DPoPNonce: response.Header.Get("DPoP-Nonce"), Expiry: expiry})
	if err != nil {
		http.Error(w, "OAuth token persistence failed", http.StatusInternalServerError)
		return
	}
	encrypted, err := c.encrypt(encoded)
	if err != nil {
		http.Error(w, "OAuth token persistence failed", http.StatusInternalServerError)
		return
	}
	if err := c.store.SaveOAuthToken(r.Context(), c.scope, bridgestore.OAuthToken{AccountDID: tokens.Sub, EncryptedPayload: encrypted, UpdatedAt: c.now().Unix()}); err != nil {
		http.Error(w, "OAuth token persistence failed", http.StatusInternalServerError)
		return
	}
	if err := c.store.DeleteOAuthSession(r.Context(), c.scope, state); err != nil {
		http.Error(w, "OAuth session cleanup failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// TokenByAccountDID returns the persisted access token and its DPoP credential.
func (c *Client) TokenByAccountDID(ctx context.Context, accountDID string) (Token, error) {
	stored, err := c.store.OAuthTokenByAccountDID(ctx, c.scope, accountDID)
	if err != nil {
		return Token{}, fmt.Errorf("load OAuth token: %w", err)
	}
	var payload tokenPayload
	if err := c.decryptJSON(stored.EncryptedPayload, &payload); err != nil {
		return Token{}, fmt.Errorf("decrypt OAuth token: %w", err)
	}
	key, err := payload.DPoPKey.ecdsa()
	if err != nil {
		return Token{}, fmt.Errorf("decode OAuth DPoP key: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return Token{}, errors.New("OAuth token has no access token")
	}
	if !payload.Expiry.IsZero() && !payload.Expiry.After(c.now()) {
		return c.refreshToken(ctx, stored.AccountDID, payload, key)
	}
	return Token{AccessToken: payload.AccessToken, RefreshToken: payload.RefreshToken, Scope: payload.Scope, DPoPKey: key, DPoPNonce: payload.DPoPNonce, Expiry: payload.Expiry}, nil
}

func (c *Client) refreshToken(ctx context.Context, accountDID string, current tokenPayload, key *ecdsa.PrivateKey) (Token, error) {
	if strings.TrimSpace(current.RefreshToken) == "" {
		return Token{}, errors.New("OAuth token has no refresh token")
	}
	metadata, err := c.discoverAuthorizationServer(ctx, c.issuer)
	if err != nil {
		return Token{}, fmt.Errorf("discover authorization server metadata: %w", err)
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {current.RefreshToken}, "client_id": {c.clientID}, "client_assertion_type": {clientAssertionType}}
	response, err := c.doDPoPFormRequest(ctx, "refresh OAuth token", metadata.TokenEndpoint, key, current.DPoPNonce, func() (url.Values, error) {
		attemptForm := cloneValues(form)
		assertion, err := c.clientAssertion(metadata.Issuer)
		if err != nil {
			return nil, err
		}
		attemptForm.Set("client_assertion", assertion)
		return attemptForm, nil
	})
	if err != nil {
		return Token{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Token{}, responseError("refresh OAuth token", response)
	}
	var refreshed tokenResponse
	if err := json.NewDecoder(response.Body).Decode(&refreshed); err != nil {
		return Token{}, fmt.Errorf("decode OAuth refresh response: %w", err)
	}
	if strings.TrimSpace(refreshed.AccessToken) == "" || !hasScope(refreshed.Scope, "atproto") {
		return Token{}, errors.New("invalid OAuth refresh response")
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = current.RefreshToken
	}
	expiry := time.Time{}
	if refreshed.ExpiresIn > 0 {
		expiry = c.now().Add(time.Duration(refreshed.ExpiresIn) * time.Second)
	}
	nonce := response.Header.Get("DPoP-Nonce")
	if nonce == "" {
		nonce = current.DPoPNonce
	}
	payload, err := json.Marshal(tokenPayload{AccessToken: refreshed.AccessToken, RefreshToken: refreshed.RefreshToken, Scope: refreshed.Scope, DPoPKey: current.DPoPKey, DPoPNonce: nonce, Expiry: expiry})
	if err != nil {
		return Token{}, fmt.Errorf("encode refreshed OAuth token: %w", err)
	}
	encrypted, err := c.encrypt(payload)
	if err != nil {
		return Token{}, err
	}
	if err := c.store.SaveOAuthToken(ctx, c.scope, bridgestore.OAuthToken{AccountDID: accountDID, EncryptedPayload: encrypted, UpdatedAt: c.now().Unix()}); err != nil {
		return Token{}, fmt.Errorf("save refreshed OAuth token: %w", err)
	}
	return Token{AccessToken: refreshed.AccessToken, RefreshToken: refreshed.RefreshToken, Scope: refreshed.Scope, DPoPKey: key, DPoPNonce: nonce, Expiry: expiry}, nil
}

func (c *Client) clientAssertion(audience string) (string, error) {
	now := c.now()
	return signJWT(c.clientSigningKey, map[string]any{"iss": c.clientID, "sub": c.clientID, "aud": audience, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "jti": mustRandomString(16)}, map[string]any{"typ": "JWT", "kid": keyID(&c.clientSigningKey.PublicKey)})
}

func (c *Client) encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(c.encryptionKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}
func (c *Client) decryptJSON(ciphertext []byte, value any) error {
	block, err := aes.NewCipher(c.encryptionKey)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return errors.New("short encrypted payload")
	}
	plaintext, err := gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(plaintext, value)
}

func responseError(operation string, response *http.Response) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return fmt.Errorf("%s: %s", operation, response.Status)
}
func hasScope(scope, want string) bool {
	for _, value := range strings.Fields(scope) {
		if value == want {
			return true
		}
	}
	return false
}
func randomString(size int) (string, error) {
	value := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
func mustRandomString(size int) string {
	value, err := randomString(size)
	if err != nil {
		panic(err)
	}
	return value
}

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d,omitempty"`
	Kid string `json:"kid,omitempty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
}

func privateJWK(key *ecdsa.PrivateKey) jwk {
	x, y := publicKeyCoordinates(&key.PublicKey)
	d, err := key.Bytes()
	if err != nil {
		panic("invalid internal P-256 private key")
	}
	return jwk{Kty: "EC", Crv: "P-256", X: x, Y: y, D: base64.RawURLEncoding.EncodeToString(d), Kid: keyID(&key.PublicKey), Use: "sig", Alg: "ES256"}
}
func publicJWK(key *ecdsa.PublicKey) jwk {
	x, y := publicKeyCoordinates(key)
	return jwk{Kty: "EC", Crv: "P-256", X: x, Y: y, Kid: keyID(key), Use: "sig", Alg: "ES256"}
}
func (key jwk) ecdsa() (*ecdsa.PrivateKey, error) {
	x, err := decodeCoordinate(key.X)
	if err != nil {
		return nil, err
	}
	y, err := decodeCoordinate(key.Y)
	if err != nil {
		return nil, err
	}
	public, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	if err != nil {
		return nil, errors.New("JWK point is invalid") //nolint:staticcheck // JWK is an initialism.
	}
	d, err := base64.RawURLEncoding.DecodeString(key.D)
	if err != nil {
		return nil, err
	}
	d = leftPad(d, 32)
	private, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d)
	if err != nil {
		return nil, err
	}
	want, _ := public.Bytes()
	got, _ := private.PublicKey.Bytes()
	if !bytes.Equal(got, want) {
		return nil, errors.New("JWK public and private keys do not match") //nolint:staticcheck // JWK is an initialism.
	}
	return private, nil
}
func decodeCoordinate(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	if len(decoded) > 32 {
		return nil, errors.New("JWK coordinate is too large") //nolint:staticcheck // JWK is an initialism.
	}
	return leftPad(decoded, 32), nil
}
func leftPad(value []byte, size int) []byte {
	if len(value) >= size {
		return value
	}
	padded := make([]byte, size)
	copy(padded[size-len(value):], value)
	return padded
}
func publicKeyCoordinates(key *ecdsa.PublicKey) (string, string) {
	encoded, err := key.Bytes()
	if err != nil || len(encoded) != 65 {
		panic("invalid internal P-256 public key")
	}
	return base64.RawURLEncoding.EncodeToString(encoded[1:33]), base64.RawURLEncoding.EncodeToString(encoded[33:])
}
func keyID(key *ecdsa.PublicKey) string {
	digest := sha256.Sum256([]byte(publicJWKWithoutKid(key)))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
func publicJWKWithoutKid(key *ecdsa.PublicKey) string {
	x, y := publicKeyCoordinates(key)
	return `{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`
}
func signJWT(key *ecdsa.PrivateKey, claims, header map[string]any) (string, error) {
	header["alg"] = "ES256"
	encodedHeader, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	encodedClaims, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(encodedHeader) + "." + base64.RawURLEncoding.EncodeToString(encodedClaims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
func dpopProof(key *ecdsa.PrivateKey, method string, requestURL *url.URL, nonce string) (string, error) {
	htu := *requestURL
	htu.RawQuery = ""
	htu.Fragment = ""
	claims := map[string]any{"jti": mustRandomString(16), "htm": method, "htu": htu.String(), "iat": time.Now().Unix()}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	return signJWT(key, claims, map[string]any{"typ": "dpop+jwt", "jwk": publicJWK(&key.PublicKey)})
}
