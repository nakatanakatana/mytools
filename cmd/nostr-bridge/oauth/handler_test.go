package oauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestHandlerStartReturnsAuthorizationURL(t *testing.T) {
	var issuer *httptest.Server
	issuer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validMetadataBody(issuer.URL)))
			return
		}
		if r.URL.Path != "/oauth/par" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("DPoP-Nonce", "nonce")
		_ = json.NewEncoder(w).Encode(map[string]string{"request_uri": "urn:test"})
	}))
	defer issuer.Close()
	client, _ := newTestClient(t, issuer.URL, issuer.Client())

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
	if err != nil || parsed.Path != "/oauth/authorize" || parsed.Query().Get("request_uri") != "urn:test" {
		t.Fatalf("authorization_url = %q", response.AuthorizationURL)
	}
}

func TestHandlerStartLogsInternalFailureAndReturnsSanitizedError(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("metadata unavailable")
	})}
	client, _ := newTestClient(t, "https://issuer.example", httpClient)

	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/oauth/start", strings.NewReader(`{"handle":"alice.test"}`))
	client.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadGateway || recorder.Body.String() != "could not start OAuth authorization\n" {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(logs.String(), "start OAuth authorization: discover authorization server metadata") {
		t.Fatalf("log = %q", logs.String())
	}
	for _, secret := range []string{"metadata unavailable", `{"handle":"alice.test"}`} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("response leaked %q", secret)
		}
	}
}

func TestHandlerStartDoesNotLogAuthorizationServerResponseBody(t *testing.T) {
	const secretResponse = `{"request_uri":"urn:secret","client_assertion":"secret-assertion"}`
	var issuer *httptest.Server
	issuer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validMetadataBody(issuer.URL)))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(secretResponse))
	}))
	defer issuer.Close()
	client, _ := newTestClient(t, issuer.URL, issuer.Client())

	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	recorder := httptest.NewRecorder()
	client.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/oauth/start", strings.NewReader(`{"handle":"alice.test"}`)))

	if recorder.Code != http.StatusBadGateway || !strings.Contains(logs.String(), "push authorization request: 400 Bad Request") {
		t.Fatalf("response = %d, log = %q", recorder.Code, logs.String())
	}
	for _, secret := range []string{"urn:secret", "secret-assertion", "request_uri", "client_assertion"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log leaked %q: %q", secret, logs.String())
		}
	}
}

func TestHandlerServesConfidentialClientMetadataAndJWKS(t *testing.T) {
	client, _ := newTestClient(t, "https://issuer.example")
	for _, test := range []struct {
		path  string
		check func(t *testing.T, body map[string]any)
	}{
		{"/oauth/client-metadata.json", func(t *testing.T, body map[string]any) {
			if body["client_id"] != client.clientID || body["token_endpoint_auth_method"] != "private_key_jwt" || body["dpop_bound_access_tokens"] != true {
				t.Fatalf("metadata = %#v", body)
			}
			assertRequestedScopes(t, body["scope"].(string))
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

func assertRequestedScopes(t *testing.T, scope string) {
	t.Helper()

	want := map[string]bool{
		"atproto": true,
		"rpc:app.bsky.graph.getFollows?aud=did:web:api.bsky.app%23bsky_appview": true,
		"rpc:app.bsky.graph.getList?aud=did:web:api.bsky.app%23bsky_appview":    true,
		"rpc:app.bsky.actor.getProfile?aud=did:web:api.bsky.app%23bsky_appview": true,
		"rpc:app.bsky.feed.getTimeline?aud=did:web:api.bsky.app%23bsky_appview": true,
	}
	got := make(map[string]bool)
	for _, value := range strings.Fields(scope) {
		got[value] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scope = %q, want permissions %#v", scope, want)
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

func TestHandlerAtServesExactProviderPrefix(t *testing.T) {
	client, _ := newTestClient(t, "https://issuer.example")
	h := client.HandlerAt("/oauth/bluesky")
	for _, route := range []string{"/oauth/bluesky/client-metadata.json", "/oauth/bluesky/jwks"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, route, nil))
		if r.Code != http.StatusOK {
			t.Fatalf("%s status = %d", route, r.Code)
		}
	}
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/oauth/jwks", nil))
	if r.Code != http.StatusNotFound {
		t.Fatalf("legacy route status = %d", r.Code)
	}
}
