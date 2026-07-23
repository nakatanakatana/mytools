package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/golden"
)

//nolint:funlen
func TestHandlerInvalidRequest(t *testing.T) {
	t.Parallel()

	filtersMap := CreateFiltersMap([]string{}, []string{})
	modifiersMap := CreateModifierMap()
	handler := createHandler(filtersMap, modifiersMap)

	testCases := []struct {
		name             string
		setupMockServer  func() (*httptest.Server, func())
		requestURL       string
		expectedStatus   int
		expectedResponse string
	}{
		{
			name: "URL parameter is required",
			setupMockServer: func() (*httptest.Server, func()) {
				return nil, func() {}
			},
			requestURL:       "/",
			expectedStatus:   http.StatusBadRequest,
			expectedResponse: "must set URL",
		},
		{
			name: "Multiple URL parameters are not allowed",
			setupMockServer: func() (*httptest.Server, func()) {
				return nil, func() {}
			},
			requestURL:       "/?url=http://example.com&url=http://another.com",
			expectedStatus:   http.StatusBadRequest,
			expectedResponse: "cannot set multiple URL",
		},
		{
			name: "Invalid feed URL should return BadRequest",
			setupMockServer: func() (*httptest.Server, func()) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte("Not Found"))
				}))

				return server, server.Close
			},
			requestURL:       "/?url=%s",
			expectedStatus:   http.StatusBadRequest,
			expectedResponse: "404",
		},
		{
			name: "Invalid XML feed should return BadRequest",
			setupMockServer: func() (*httptest.Server, func()) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("This is not a valid XML feed"))
				}))

				return server, server.Close
			},
			requestURL:       "/?url=%s",
			expectedStatus:   http.StatusBadRequest,
			expectedResponse: "ParseURL Error",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockServer, cleanup := tc.setupMockServer()
			defer cleanup()

			requestURL := tc.requestURL
			if mockServer != nil {
				requestURL = strings.ReplaceAll(requestURL, "%s", mockServer.URL)
			}

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, requestURL, nil)
			rec := httptest.NewRecorder()

			handler(rec, req)

			assert.Equal(t, tc.expectedStatus, rec.Code)
			assert.Assert(t, cmp.Contains(rec.Body.String(), tc.expectedResponse))
		})
	}
}

//nolint:funlen
func TestHandlerSuccess(t *testing.T) {
	t.Parallel()

	filtersMap := CreateFiltersMap([]string{}, []string{})
	modifiersMap := CreateModifierMap()
	handler := createHandler(filtersMap, modifiersMap)

	testCases := []struct {
		name           string
		requestURL     string
		expectedStatus int
		goldenFile     string
	}{
		{
			name:           "Valid RSS feed should return OK",
			requestURL:     "/?url=%s",
			expectedStatus: http.StatusOK,
			goldenFile:     "valid-rss-feed",
		},
		{
			name:           "Valid RSS feed with filter query should return filtered feed",
			requestURL:     "/?url=%s&title.contains=Second",
			expectedStatus: http.StatusOK,
			goldenFile:     "filtered-rss-feed",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0">
<channel>
  <title>RSS Title</title>
  <link>http://example.com</link>
  <description>This is an example RSS feed</description>
  <item>
    <title>Example entry</title>
    <link>http://example.com/1</link>
    <description>Example description</description>
  </item>
  <item>
    <title>Second entry</title>
    <link>http://example.com/2</link>
    <description>Second description</description>
  </item>
</channel>
</rss>`))
			}))
			defer mockServer.Close()

			requestURL := strings.ReplaceAll(tc.requestURL, "%s", mockServer.URL)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, requestURL, nil)
			rec := httptest.NewRecorder()

			handler(rec, req)

			assert.Equal(t, tc.expectedStatus, rec.Code)
			golden.Assert(t, rec.Body.String(), tc.goldenFile)
		})
	}
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	t.Parallel()

	handler := createHandler(CreateFiltersMap(nil, nil), CreateModifierMap())

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/?url=http://example.com", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandlerHeadRequest(t *testing.T) {
	t.Parallel()

	handler := createHandler(CreateFiltersMap(nil, nil), CreateModifierMap())

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0">
<channel>
  <title>RSS Title</title>
</channel>
</rss>`))
	}))
	defer mockServer.Close()

	requestURL := "/?url=" + mockServer.URL
	req := httptest.NewRequestWithContext(context.Background(), http.MethodHead, requestURL, nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "", rec.Body.String())
}

func TestHandlerWithMultipleQueries(t *testing.T) {
	t.Parallel()

	handler := createHandler(CreateFiltersMap(nil, nil), CreateModifierMap())

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0">
<channel>
  <title>RSS Title</title>
  <item><title>First item</title><description>Keep this</description></item>
  <item><title>Second item</title><description>Remove this</description></item>
</channel>
</rss>`))
	}))
	defer mockServer.Close()

	requestURL := "/?url=%s&title.contains=First&description.not_contains=Remove"
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		strings.ReplaceAll(requestURL, "%s", mockServer.URL), nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Assert(t, cmp.Contains(rec.Body.String(), "First item"))
	assert.Assert(t, !strings.Contains(rec.Body.String(), "Second item"))
}
