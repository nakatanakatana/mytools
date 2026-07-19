package oauth

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// Handler serves the OAuth start/callback routes and the public client metadata.
func (c *Client) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/start", c.handleStart)
	mux.HandleFunc("GET /oauth/callback", c.HandleCallback)
	mux.HandleFunc("GET "+metadataPath(c.clientID), c.handleMetadata)
	mux.HandleFunc("GET /oauth/jwks", c.handleJWKS)
	return mux
}

func (c *Client) handleStart(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Handle string `json:"handle"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil || strings.TrimSpace(request.Handle) == "" {
		http.Error(w, "handle is required", http.StatusBadRequest)
		return
	}
	authorizationURL, err := c.StartAuthorization(r.Context(), request.Handle)
	if err != nil {
		http.Error(w, "could not start OAuth authorization", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		AuthorizationURL string `json:"authorization_url"`
	}{authorizationURL})
}

func (c *Client) handleMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"client_id": c.clientID, "application_type": "web", "grant_types": []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"}, "scope": "atproto", "redirect_uris": []string{c.redirectURL},
		"token_endpoint_auth_method": "private_key_jwt", "token_endpoint_auth_signing_alg": "ES256",
		"dpop_bound_access_tokens": true, "jwks_uri": jwksURL(c.clientID),
	})
}

func (c *Client) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []jwk{publicJWK(&c.clientSigningKey.PublicKey)}})
}

func metadataPath(clientID string) string {
	parsed, err := url.Parse(clientID)
	if err != nil || parsed.Path == "" {
		return "/oauth/client-metadata.json"
	}
	return parsed.Path
}
func jwksURL(clientID string) string {
	parsed, err := url.Parse(clientID)
	if err != nil {
		return "/oauth/jwks"
	}
	parsed.Path = path.Join(path.Dir(parsed.Path), "jwks")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}
