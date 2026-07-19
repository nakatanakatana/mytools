// Package mastodon implements Mastodon source integration.
package mastodon

import (
	"context"
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

	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/secretbox"
	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

const OAuthScopes = "profile read:accounts read:follows read:lists read:statuses read:notifications"

type OAuthOptions struct {
	Scope                                                 bridgestore.SourceScope
	Store                                                 bridgestore.OAuthStore
	HTTPClient                                            *http.Client
	BaseURL, Account, ClientID, ClientSecret, RedirectURL string
	EncryptionKey                                         []byte
	Now                                                   func() time.Time
}
type OAuthClient struct {
	scope                                                 bridgestore.SourceScope
	store                                                 bridgestore.OAuthStore
	httpClient                                            *http.Client
	baseURL, account, clientID, clientSecret, redirectURL string
	box                                                   secretbox.Box
	now                                                   func() time.Time
}
type oauthSession struct {
	CodeVerifier string `json:"code_verifier"`
}
type oauthTokenPayload struct {
	AccessToken, RefreshToken, Scope string
	Expiry                           time.Time
}
type mastodonTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
}
type Token struct {
	AccessToken, RefreshToken, Scope string
	Expiry                           time.Time
}

func NewOAuthClient(o OAuthOptions) (*OAuthClient, error) {
	if o.Store == nil || strings.TrimSpace(o.BaseURL) == "" || strings.TrimSpace(o.Account) == "" || strings.TrimSpace(o.ClientID) == "" || strings.TrimSpace(o.ClientSecret) == "" || strings.TrimSpace(o.RedirectURL) == "" {
		return nil, errors.New("Mastodon OAuth requires store, base URL, account, client credentials, and redirect URL")
	}
	box, err := secretbox.New(o.EncryptionKey)
	if err != nil {
		return nil, err
	}
	if o.HTTPClient == nil {
		o.HTTPClient = http.DefaultClient
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	account := normalizeAccount(o.Account, instanceHost(o.BaseURL))
	if o.Scope.Provider != "mastodon" || o.Scope.Account != account {
		return nil, errors.New("Mastodon OAuth scope must exactly match the normalized configured account")
	}
	return &OAuthClient{scope: o.Scope, store: o.Store, httpClient: o.HTTPClient, baseURL: strings.TrimRight(o.BaseURL, "/"), account: account, clientID: o.ClientID, clientSecret: o.ClientSecret, redirectURL: o.RedirectURL, box: box, now: o.Now}, nil
}

func (c *OAuthClient) StartAuthorization(ctx context.Context) (string, error) {
	state, err := randomOAuthString(32)
	if err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	verifier, err := randomOAuthString(48)
	if err != nil {
		return "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	encoded, _ := json.Marshal(oauthSession{CodeVerifier: verifier})
	encrypted, err := c.box.Seal(encoded)
	if err != nil {
		return "", errors.New("encrypt OAuth session")
	}
	if err := c.store.SaveOAuthSession(ctx, c.scope, bridgestore.OAuthSession{State: state, EncryptedPayload: encrypted, ExpiresAt: c.now().Add(10 * time.Minute).Unix()}); err != nil {
		return "", fmt.Errorf("save OAuth session: %w", err)
	}
	h := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {c.clientID}, "redirect_uri": {c.redirectURL}, "response_type": {"code"}, "scope": {OAuthScopes}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(h[:])}, "code_challenge_method": {"S256"}}
	return c.baseURL + "/oauth/authorize?" + q.Encode(), nil
}
func (c *OAuthClient) openSession(encrypted []byte) (oauthSession, error) {
	plain, err := c.box.Open(encrypted)
	if err != nil {
		return oauthSession{}, err
	}
	var p oauthSession
	err = json.Unmarshal(plain, &p)
	return p, err
}

func (c *OAuthClient) HandleCallback(w http.ResponseWriter, r *http.Request) {
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if state == "" || code == "" {
		http.Error(w, "invalid OAuth callback", http.StatusBadRequest)
		return
	}
	session, err := c.store.OAuthSessionByState(r.Context(), c.scope, state)
	if err != nil {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	if session.ExpiresAt <= c.now().Unix() {
		_ = c.store.DeleteOAuthSession(r.Context(), c.scope, state)
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	payload, err := c.openSession(session.EncryptedPayload)
	if err != nil || payload.CodeVerifier == "" {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	tokens, err := c.exchange(r.Context(), url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {c.redirectURL}, "client_id": {c.clientID}, "client_secret": {c.clientSecret}, "code_verifier": {payload.CodeVerifier}, "scope": {OAuthScopes}})
	if err != nil {
		http.Error(w, "OAuth token exchange failed", http.StatusBadGateway)
		return
	}
	acct, err := c.verifyCredentials(r.Context(), tokens.AccessToken)
	if err != nil {
		http.Error(w, "OAuth account verification failed", http.StatusBadGateway)
		return
	}
	if normalizeAccount(acct, instanceHost(c.baseURL)) != c.account {
		_ = c.store.DeleteOAuthSession(r.Context(), c.scope, state)
		http.Error(w, "OAuth account does not match configured account", http.StatusForbidden)
		return
	}
	if err := c.saveToken(r.Context(), tokens, c.account); err != nil {
		http.Error(w, "OAuth token persistence failed", http.StatusInternalServerError)
		return
	}
	if err := c.store.DeleteOAuthSession(r.Context(), c.scope, state); err != nil {
		http.Error(w, "OAuth session cleanup failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (c *OAuthClient) exchange(ctx context.Context, form url.Values) (mastodonTokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return mastodonTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return mastodonTokenResponse{}, errors.New("Mastodon OAuth request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return mastodonTokenResponse{}, errors.New("Mastodon OAuth server rejected request")
	}
	var token mastodonTokenResponse
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token) != nil || strings.TrimSpace(token.AccessToken) == "" || !exactScopes(token.Scope) {
		return token, errors.New("invalid Mastodon OAuth token response")
	}
	return token, nil
}
func (c *OAuthClient) verifyCredentials(ctx context.Context, access string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/accounts/verify_credentials", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", errors.New("Mastodon account request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("Mastodon account request rejected")
	}
	var account struct {
		Acct string `json:"acct"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&account) != nil || account.Acct == "" {
		return "", errors.New("invalid Mastodon account response")
	}
	return account.Acct, nil
}
func (c *OAuthClient) saveToken(ctx context.Context, t mastodonTokenResponse, id string) error {
	expiry := time.Time{}
	if t.ExpiresIn > 0 {
		expiry = c.now().Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	plain, err := json.Marshal(oauthTokenPayload{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, Scope: t.Scope, Expiry: expiry})
	if err != nil {
		return err
	}
	encrypted, err := c.box.Seal(plain)
	if err != nil {
		return err
	}
	return c.store.SaveOAuthToken(ctx, c.scope, bridgestore.OAuthToken{AccountDID: id, EncryptedPayload: encrypted, UpdatedAt: c.now().Unix()})
}
func (c *OAuthClient) Token(ctx context.Context) (Token, error) {
	stored, err := c.store.OAuthTokenByAccountDID(ctx, c.scope, c.account)
	if err != nil {
		return Token{}, fmt.Errorf("load Mastodon OAuth token: %w", err)
	}
	plain, err := c.box.Open(stored.EncryptedPayload)
	if err != nil {
		return Token{}, errors.New("decrypt Mastodon OAuth token")
	}
	var p oauthTokenPayload
	if json.Unmarshal(plain, &p) != nil || p.AccessToken == "" {
		return Token{}, errors.New("invalid Mastodon OAuth token")
	}
	if p.Expiry.IsZero() || p.Expiry.After(c.now()) {
		return Token{p.AccessToken, p.RefreshToken, p.Scope, p.Expiry}, nil
	}
	if p.RefreshToken == "" {
		return Token{}, errors.New("Mastodon OAuth token has no refresh token")
	}
	fresh, err := c.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {p.RefreshToken}, "client_id": {c.clientID}, "client_secret": {c.clientSecret}, "scope": {OAuthScopes}})
	if err != nil {
		return Token{}, err
	}
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = p.RefreshToken
	}
	if err := c.saveToken(ctx, fresh, c.account); err != nil {
		return Token{}, errors.New("save refreshed Mastodon OAuth token")
	}
	expiry := time.Time{}
	if fresh.ExpiresIn > 0 {
		expiry = c.now().Add(time.Duration(fresh.ExpiresIn) * time.Second)
	}
	return Token{fresh.AccessToken, fresh.RefreshToken, fresh.Scope, expiry}, nil
}
func exactScopes(raw string) bool {
	got := strings.Fields(raw)
	want := strings.Fields(OAuthScopes)
	if len(got) != len(want) {
		return false
	}
	set := map[string]bool{}
	for _, v := range got {
		set[v] = true
	}
	for _, v := range want {
		if !set[v] {
			return false
		}
	}
	return true
}
func instanceHost(raw string) string { u, _ := url.Parse(raw); return strings.ToLower(u.Hostname()) }
func normalizeAccount(acct, host string) string {
	acct = strings.TrimSpace(strings.TrimPrefix(acct, "@"))
	if !strings.Contains(acct, "@") {
		acct += "@" + host
	}
	return strings.ToLower(acct)
}
func randomOAuthString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
