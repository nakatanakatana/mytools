package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// MockJSONProvider implements JSONProvider for testing
type MockJSONProvider struct {
	responses map[string][]byte
}

func (m *MockJSONProvider) GetJSON(name string) []byte {
	return m.responses[name]
}

func TestNIP05Handler(t *testing.T) {
	fullJSON := `{"names":{"alice":"pub1","bob":"pub2"}}`
	aliceJSON := `{"names":{"alice":"pub1"}}`

	provider := &MockJSONProvider{
		responses: map[string][]byte{
			"":      []byte(fullJSON),
			"alice": []byte(aliceJSON),
		},
	}

	handler := NewNIP05Handler(provider)

	tests := []struct {
		name           string
		queryName      string
		hasQuery       bool
		wantStatusCode int
		wantBody       string
	}{
		{
			name:           "Full List",
			queryName:      "",
			hasQuery:       false,
			wantStatusCode: http.StatusOK,
			wantBody:       fullJSON,
		},
		{
			name:           "Individual User",
			queryName:      "alice",
			hasQuery:       true,
			wantStatusCode: http.StatusOK,
			wantBody:       aliceJSON,
		},
		{
			name:           "Unknown User",
			queryName:      "unknown",
			hasQuery:       true,
			wantStatusCode: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := "/.well-known/nostr.json"
			if tt.hasQuery {
				url += "?name=" + tt.queryName
			}
			req, _ := http.NewRequest("GET", url, nil)
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatusCode {
				t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, tt.wantStatusCode)
			}

			if tt.wantStatusCode == http.StatusOK {
				if contentType := rr.Header().Get("Content-Type"); contentType != "application/json; charset=utf-8" {
					t.Errorf("handler returned wrong Content-Type: got %v want %v", contentType, "application/json; charset=utf-8")
				}
				if rr.Body.String() != tt.wantBody {
					t.Errorf("handler returned unexpected body: got %v want %v", rr.Body.String(), tt.wantBody)
				}
			}
		})
	}
}