// Package mastodon implements Mastodon source integration.
package mastodon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

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

type oauthRemoteError struct {
	operation, code, description string
	statusCode                   int
}

type oauthTransportError struct {
	class, detail string
}

func (e oauthTransportError) Error() string {
	return fmt.Sprintf("mastodon OAuth transport failed: class=%s detail=%q", e.class, e.detail)
}

func newOAuthTransportError(err error) error {
	class, detail := "other_transport_error", "request failed before receiving an HTTP response"
	for current := err; current != nil; current = errors.Unwrap(current) {
		if urlError, ok := current.(*url.Error); ok && strings.Contains(strings.ToLower(urlError.Op), "proxy") {
			return oauthTransportError{class: "proxy_error", detail: "proxy connection failed"}
		}
	}
	if errors.Is(err, context.Canceled) {
		return oauthTransportError{class: "context_canceled", detail: "request context was canceled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return oauthTransportError{class: "deadline_exceeded", detail: "request deadline was exceeded"}
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return oauthTransportError{class: "dns_error", detail: "DNS lookup failed"}
	}
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
		return oauthTransportError{class: "network_unreachable", detail: "network or host is unreachable"}
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return oauthTransportError{class: "connection_refused", detail: "connection was refused"}
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return oauthTransportError{class: "connection_reset", detail: "connection was reset"}
	}
	var unknownAuthority x509.UnknownAuthorityError
	var certificateInvalid x509.CertificateInvalidError
	var hostnameInvalid x509.HostnameError
	var recordHeader tls.RecordHeaderError
	if errors.As(err, &unknownAuthority) || errors.As(err, &certificateInvalid) || errors.As(err, &hostnameInvalid) || errors.As(err, &recordHeader) {
		return oauthTransportError{class: "tls_error", detail: "TLS certificate or protocol validation failed"}
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return oauthTransportError{class: "deadline_exceeded", detail: "network operation timed out"}
	}
	return oauthTransportError{class: class, detail: detail}
}

func (e oauthRemoteError) Error() string {
	message := fmt.Sprintf("Mastodon OAuth %s failed: status=%d", e.operation, e.statusCode)
	if e.code != "" {
		message += fmt.Sprintf(" error=%q", e.code)
	}
	if e.description != "" {
		message += fmt.Sprintf(" description=%q", e.description)
	}
	return message
}

func newOAuthRemoteError(operation string, response *http.Response, secrets ...string) error {
	var details struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&details)
	return oauthRemoteError{
		operation: operation, statusCode: response.StatusCode,
		code: safeOAuthDetail(details.Error, secrets...), description: safeOAuthDetail(details.ErrorDescription, secrets...),
	}
}

func safeOAuthDetail(value string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	var safe strings.Builder
	for _, character := range value {
		if unicode.IsControl(character) {
			continue
		}
		size := utf8.RuneLen(character)
		if safe.Len()+size > 256 {
			break
		}
		safe.WriteRune(character)
	}
	return safe.String()
}

func logOAuthResult(stage, result string) {
	log.Printf("nostr-bridge OAuth: provider=mastodon stage=%s result=%s", stage, result)
}

func logOAuthFailure(stage string, err error, secrets ...string) {
	detail := ""
	if err != nil {
		detail = safeOAuthDetail(err.Error(), secrets...)
	}
	log.Printf("nostr-bridge OAuth: provider=mastodon stage=%s result=failed error=%q", stage, detail)
}

func NewOAuthClient(o OAuthOptions) (*OAuthClient, error) {
	if o.Store == nil || strings.TrimSpace(o.BaseURL) == "" || strings.TrimSpace(o.Account) == "" || strings.TrimSpace(o.ClientID) == "" || strings.TrimSpace(o.ClientSecret) == "" || strings.TrimSpace(o.RedirectURL) == "" {
		return nil, errors.New("mastodon OAuth requires store, base URL, account, client credentials, and redirect URL")
	}
	box, err := secretbox.New(o.EncryptionKey)
	if err != nil {
		return nil, err
	}
	if o.HTTPClient == nil {
		o.HTTPClient = newHTTPClient()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	account := normalizeAccount(o.Account, instanceHost(o.BaseURL))
	if o.Scope.Provider != "mastodon" || o.Scope.Account != account {
		return nil, errors.New("mastodon OAuth scope must exactly match the normalized configured account")
	}
	return &OAuthClient{scope: o.Scope, store: o.Store, httpClient: o.HTTPClient, baseURL: strings.TrimRight(o.BaseURL, "/"), account: account, clientID: o.ClientID, clientSecret: o.ClientSecret, redirectURL: o.RedirectURL, box: box, now: o.Now}, nil
}

func (c *OAuthClient) StartAuthorization(ctx context.Context) (string, error) {
	logOAuthResult("authorization_start", "started")
	state, err := randomOAuthString(32)
	if err != nil {
		logOAuthFailure("state_generation", err)
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	verifier, err := randomOAuthString(48)
	if err != nil {
		logOAuthFailure("verifier_generation", err, state)
		return "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	encoded, _ := json.Marshal(oauthSession{CodeVerifier: verifier})
	encrypted, err := c.box.Seal(encoded)
	if err != nil {
		logOAuthFailure("session_encryption", err, state, verifier)
		return "", errors.New("encrypt OAuth session")
	}
	if err := c.store.SaveOAuthSession(ctx, c.scope, bridgestore.OAuthSession{State: state, EncryptedPayload: encrypted, ExpiresAt: c.now().Add(10 * time.Minute).Unix()}); err != nil {
		logOAuthFailure("session_persistence", err, state, verifier)
		return "", fmt.Errorf("save OAuth session: %w", err)
	}
	h := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {c.clientID}, "redirect_uri": {c.redirectURL}, "response_type": {"code"}, "scope": {OAuthScopes}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(h[:])}, "code_challenge_method": {"S256"}}
	logOAuthResult("authorization_start", "succeeded")
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
	logOAuthResult("callback", "started")
	if state == "" || code == "" {
		logOAuthFailure("callback_validation", errors.New("required callback parameters are missing"), state, code)
		http.Error(w, "invalid OAuth callback", http.StatusBadRequest)
		return
	}
	session, err := c.store.OAuthSessionByState(r.Context(), c.scope, state)
	if err != nil {
		logOAuthFailure("state_lookup", err, state, code)
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	if session.ExpiresAt <= c.now().Unix() {
		if deleteErr := c.store.DeleteOAuthSession(r.Context(), c.scope, state); deleteErr != nil {
			logOAuthFailure("expired_state_cleanup", deleteErr, state, code)
		}
		logOAuthFailure("state_validation", errors.New("OAuth state expired"), state, code)
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	payload, err := c.openSession(session.EncryptedPayload)
	if err != nil || payload.CodeVerifier == "" {
		if err == nil {
			err = errors.New("OAuth state has no PKCE verifier")
		}
		logOAuthFailure("state_decryption", err, state, code)
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	tokens, err := c.exchange(r.Context(), url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {c.redirectURL}, "client_id": {c.clientID}, "client_secret": {c.clientSecret}, "code_verifier": {payload.CodeVerifier}, "scope": {OAuthScopes}})
	if err != nil {
		logOAuthFailure("token_exchange", err, state, code, c.clientSecret, payload.CodeVerifier)
		http.Error(w, "OAuth token exchange failed", http.StatusBadGateway)
		return
	}
	acct, err := c.verifyCredentials(r.Context(), tokens.AccessToken)
	if err != nil {
		logOAuthFailure("credential_verification", err, state, code, tokens.AccessToken, tokens.RefreshToken)
		http.Error(w, "OAuth account verification failed", http.StatusBadGateway)
		return
	}
	if normalizeAccount(acct, instanceHost(c.baseURL)) != c.account {
		if deleteErr := c.store.DeleteOAuthSession(r.Context(), c.scope, state); deleteErr != nil {
			logOAuthFailure("mismatched_state_cleanup", deleteErr, state, code, tokens.AccessToken, tokens.RefreshToken)
		}
		logOAuthFailure("account_verification", errors.New("authorized account does not match configured account"), state, code, tokens.AccessToken, tokens.RefreshToken)
		http.Error(w, "OAuth account does not match configured account", http.StatusForbidden)
		return
	}
	if err := c.saveToken(r.Context(), tokens, c.account); err != nil {
		logOAuthFailure("token_persistence", err, state, code, tokens.AccessToken, tokens.RefreshToken)
		http.Error(w, "OAuth token persistence failed", http.StatusInternalServerError)
		return
	}
	if err := c.store.DeleteOAuthSession(r.Context(), c.scope, state); err != nil {
		logOAuthFailure("session_cleanup", err, state, code, tokens.AccessToken, tokens.RefreshToken)
		http.Error(w, "OAuth session cleanup failed", http.StatusInternalServerError)
		return
	}
	logOAuthResult("complete", "succeeded")
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
		return mastodonTokenResponse{}, newOAuthTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return mastodonTokenResponse{}, newOAuthRemoteError("token exchange", resp, form.Get("code"), form.Get("client_secret"), form.Get("refresh_token"), form.Get("code_verifier"))
	}
	var token mastodonTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token); err != nil {
		return token, errors.New("decode Mastodon OAuth token response")
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return token, errors.New("mastodon OAuth token response is missing access token")
	}
	if !exactScopes(token.Scope) {
		return token, errors.New("mastodon OAuth token response scope mismatch")
	}
	return token, nil
}
func (c *OAuthClient) verifyCredentials(ctx context.Context, access string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/accounts/verify_credentials", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", newOAuthTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", newOAuthRemoteError("credential verification", resp, access)
	}
	var account struct {
		Acct string `json:"acct"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&account); err != nil {
		return "", errors.New("decode Mastodon account response")
	}
	if account.Acct == "" {
		return "", errors.New("mastodon account response is missing account")
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
		return Token(p), nil
	}
	if p.RefreshToken == "" {
		return Token{}, errors.New("mastodon OAuth token has no refresh token")
	}
	logOAuthResult("token_refresh", "started")
	fresh, err := c.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {p.RefreshToken}, "client_id": {c.clientID}, "client_secret": {c.clientSecret}, "scope": {OAuthScopes}})
	if err != nil {
		logOAuthFailure("token_refresh", err, p.AccessToken, p.RefreshToken, c.clientSecret)
		return Token{}, err
	}
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = p.RefreshToken
	}
	if err := c.saveToken(ctx, fresh, c.account); err != nil {
		logOAuthFailure("token_refresh_persistence", err, p.AccessToken, p.RefreshToken, fresh.AccessToken, fresh.RefreshToken, c.clientSecret)
		return Token{}, errors.New("save refreshed Mastodon OAuth token")
	}
	expiry := time.Time{}
	if fresh.ExpiresIn > 0 {
		expiry = c.now().Add(time.Duration(fresh.ExpiresIn) * time.Second)
	}
	logOAuthResult("token_refresh", "succeeded")
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
