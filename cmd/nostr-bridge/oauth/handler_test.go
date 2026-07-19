package oauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHandlerStartReturnsAuthorizationURL(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/par" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("DPoP-Nonce", "nonce")
		_ = json.NewEncoder(w).Encode(map[string]string{"request_uri": "urn:test"})
	}))
	defer issuer.Close()
	client, _ := newTestClient(t, issuer.URL)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/oauth/start", strings.NewReader(`{"handle":"alice.test"}`))
	request.Header.Set("Content-Type", "application/json")
	client.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(response.AuthorizationURL)
	if err != nil || parsed.Path != "/authorize" || parsed.Query().Get("request_uri") != "urn:test" {
		t.Fatalf("authorization_url = %q", response.AuthorizationURL)
	}
}

func TestHandlerServesConfidentialClientMetadataAndJWKS(t *testing.T) {
	client, _ := newTestClient(t, "https://issuer.example")
	for _, test := range []struct {
		path  string
		check func(t *testing.T, body map[string]any)
	}{
		{"/oauth/client-metadata.json", func(t *testing.T, body map[string]any) {
			if body["client_id"] != client.clientID || body["token_endpoint_auth_method"] != "private_key_jwt" || body["dpop_bound_access_tokens"] != true || body["scope"] != "atproto" {
				t.Fatalf("metadata = %#v", body)
			}
			if !containsString(body["grant_types"], "authorization_code") || !containsString(body["grant_types"], "refresh_token") || !containsString(body["response_types"], "code") || body["jwks_uri"] == "" {
				t.Fatalf("metadata = %#v", body)
			}
		}},
		{"/oauth/jwks", func(t *testing.T, body map[string]any) {
			keys, ok := body["keys"].([]any)
			if !ok || len(keys) != 1 {
				t.Fatalf("JWKS = %#v", body)
			}
			key, ok := keys[0].(map[string]any)
			if !ok || key["kty"] != "EC" || key["alg"] != "ES256" {
				t.Fatalf("JWKS = %#v", body)
			}
		}},
	} {
		t.Run(test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			client.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d", recorder.Code)
			}
			var body map[string]any
			if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			test.check(t, body)
		})
	}
}

func containsString(value any, wanted string) bool {
	values, ok := value.([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		if stringValue, ok := value.(string); ok && stringValue == wanted {
			return true
		}
	}
	return false
}
