package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// MockFilePathProvider implements FilePathProvider for testing
type MockFilePathProvider struct {
	files map[string]string
}

func (m *MockFilePathProvider) GetFilePath(name string) string {
	return m.files[name]
}

func TestNIP05Handler(t *testing.T) {
	// Setup temporary files for testing
	tempDir, err := os.MkdirTemp("", "handler_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	fullJSON := `{"names":{"alice":"pub1","bob":"pub2"}}`
	aliceJSON := `{"names":{"alice":"pub1"}}`
	fullPath := filepath.Join(tempDir, "full.json")
	alicePath := filepath.Join(tempDir, "alice.json")

	if err := os.WriteFile(fullPath, []byte(fullJSON), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alicePath, []byte(aliceJSON), 0644); err != nil {
		t.Fatal(err)
	}

	provider := &MockFilePathProvider{
		files: map[string]string{
			"":      fullPath,
			"alice": alicePath,
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
				if rr.Body.String() != tt.wantBody {
					t.Errorf("handler returned unexpected body: got %v want %v", rr.Body.String(), tt.wantBody)
				}
			}
		})
	}
}