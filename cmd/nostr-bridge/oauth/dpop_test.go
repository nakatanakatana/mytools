package oauth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDPoPFormRequestLimitsNonceChallengeRetries(t *testing.T) {
	tests := []struct {
		name            string
		errorCode       string
		addNonceHeaders func(http.Header)
		wantRequests    int
		wantPresent     bool
	}{
		{name: "missing nonce header", errorCode: "use_dpop_nonce", addNonceHeaders: func(http.Header) {}, wantRequests: 1, wantPresent: false},
		{name: "empty nonce header", errorCode: "use_dpop_nonce", addNonceHeaders: func(header http.Header) {
			header.Set("DPoP-Nonce", "")
		}, wantRequests: 1, wantPresent: true},
		{name: "multiple nonce headers", errorCode: "use_dpop_nonce", addNonceHeaders: func(header http.Header) {
			header.Add("DPoP-Nonce", "first-secret")
			header.Add("DPoP-Nonce", "second-secret")
		}, wantRequests: 1, wantPresent: true},
		{name: "different OAuth error", errorCode: "invalid_request", addNonceHeaders: func(header http.Header) {
			header.Set("DPoP-Nonce", "retry-secret")
		}, wantRequests: 1, wantPresent: true},
		{name: "second nonce challenge", errorCode: "use_dpop_nonce", addNonceHeaders: func(header http.Header) {
			header.Set("DPoP-Nonce", "retry-secret")
		}, wantRequests: 2, wantPresent: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				test.addNonceHeaders(w.Header())
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": test.errorCode})
			}))
			defer server.Close()
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			client := &Client{httpClient: server.Client()}

			var logs bytes.Buffer
			previousOutput := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(previousOutput) })

			response, err := client.doDPoPFormRequest(context.Background(), "test request", server.URL, key, "", func() (url.Values, error) {
				return url.Values{"client_id": {"test-client"}}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if requests != test.wantRequests {
				t.Fatalf("requests = %d, want %d", requests, test.wantRequests)
			}
			wantLog := "DPoP-Nonce header present=" + map[bool]string{true: "true", false: "false"}[test.wantPresent]
			if !strings.Contains(logs.String(), wantLog) {
				t.Fatalf("log = %q, want %q", logs.String(), wantLog)
			}
			for _, secret := range []string{"first-secret", "second-secret", "retry-secret"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("log leaked nonce: %q", logs.String())
				}
			}
		})
	}
}
