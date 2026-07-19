package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoverAuthorizationServer(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/.well-known/oauth-authorization-server" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(authorizationServerMetadata{
			Issuer:                             server.URL,
			AuthorizationEndpoint:              server.URL + "/oauth/authorize",
			TokenEndpoint:                      server.URL + "/oauth/token",
			PushedAuthorizationRequestEndpoint: server.URL + "/oauth/par",
			RequirePushedAuthorizationRequests: true,
		})
	}))
	defer server.Close()

	client := &Client{httpClient: server.Client()}
	metadata, err := client.discoverAuthorizationServer(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Issuer != server.URL ||
		metadata.AuthorizationEndpoint != server.URL+"/oauth/authorize" ||
		metadata.TokenEndpoint != server.URL+"/oauth/token" ||
		metadata.PushedAuthorizationRequestEndpoint != server.URL+"/oauth/par" ||
		!metadata.RequirePushedAuthorizationRequests {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestDiscoverAuthorizationServerRejectsInvalidMetadata(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        func(string) string
		wantError   string
	}{
		{name: "non-200 status", status: http.StatusServiceUnavailable, contentType: "application/json", body: validMetadataBody, wantError: "status 503"},
		{name: "non-JSON media type", status: http.StatusOK, contentType: "text/plain", body: validMetadataBody, wantError: "Content-Type"},
		{name: "malformed JSON", status: http.StatusOK, contentType: "application/json", body: func(string) string { return `{` }, wantError: "decode"},
		{name: "trailing JSON", status: http.StatusOK, contentType: "application/json", body: func(issuer string) string { return validMetadataBody(issuer) + `{}` }, wantError: "trailing"},
		{name: "oversized body", status: http.StatusOK, contentType: "application/json", body: func(string) string { return `{"padding":"` + strings.Repeat("x", 32<<10) + `"}` }, wantError: "too large"},
		{name: "mismatched issuer", status: http.StatusOK, contentType: "application/json", body: func(issuer string) string {
			return metadataBody(issuer, func(metadata *authorizationServerMetadata) { metadata.Issuer = "https://other.example" })
		}, wantError: "issuer mismatch"},
		{name: "missing endpoint", status: http.StatusOK, contentType: "application/json", body: func(issuer string) string {
			return metadataBody(issuer, func(metadata *authorizationServerMetadata) { metadata.TokenEndpoint = "" })
		}, wantError: "token_endpoint"},
		{name: "HTTP endpoint", status: http.StatusOK, contentType: "application/json", body: func(issuer string) string {
			return metadataBody(issuer, func(metadata *authorizationServerMetadata) {
				metadata.AuthorizationEndpoint = "http://example.com/authorize"
			})
		}, wantError: "authorization_endpoint"},
		{name: "relative endpoint", status: http.StatusOK, contentType: "application/json", body: func(issuer string) string {
			return metadataBody(issuer, func(metadata *authorizationServerMetadata) {
				metadata.PushedAuthorizationRequestEndpoint = "/oauth/par"
			})
		}, wantError: "pushed_authorization_request_endpoint"},
		{name: "endpoint userinfo", status: http.StatusOK, contentType: "application/json", body: func(issuer string) string {
			return metadataBody(issuer, func(metadata *authorizationServerMetadata) { metadata.TokenEndpoint = "https://user@example.com/token" })
		}, wantError: "token_endpoint"},
		{name: "endpoint fragment", status: http.StatusOK, contentType: "application/json", body: func(issuer string) string {
			return metadataBody(issuer, func(metadata *authorizationServerMetadata) {
				metadata.TokenEndpoint = "https://example.com/token#fragment"
			})
		}, wantError: "token_endpoint"},
		{name: "PAR not required", status: http.StatusOK, contentType: "application/json", body: func(issuer string) string {
			return metadataBody(issuer, func(metadata *authorizationServerMetadata) { metadata.RequirePushedAuthorizationRequests = false })
		}, wantError: "require_pushed_authorization_requests"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body(server.URL)))
			}))
			defer server.Close()

			client := &Client{httpClient: server.Client()}
			_, err := client.discoverAuthorizationServer(context.Background(), server.URL)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func validMetadataBody(issuer string) string {
	return metadataBody(issuer, func(*authorizationServerMetadata) {})
}

func metadataBody(issuer string, mutate func(*authorizationServerMetadata)) string {
	metadata := authorizationServerMetadata{
		Issuer:                             issuer,
		AuthorizationEndpoint:              issuer + "/oauth/authorize",
		TokenEndpoint:                      issuer + "/oauth/token",
		PushedAuthorizationRequestEndpoint: issuer + "/oauth/par",
		RequirePushedAuthorizationRequests: true,
	}
	mutate(&metadata)
	encoded, err := json.Marshal(metadata)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
