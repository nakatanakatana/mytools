package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/fs"
)

const (
	testETagKey   = "test-etag-key.rss"
	testETagValue = `"abc123"`
)

func TestCacheMiddleware(t *testing.T) {
	t.Parallel()

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("test response"))
	})

	middleware, err := NewCacheMiddleware(testHandler)
	assert.NilError(t, err, "Failed to create cache middleware")

	params := url.Values{}
	params.Set("url", "https://example.com/feed")
	params.Set("title.contains", "test")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/?"+params.Encode(), nil)
	w := httptest.NewRecorder()

	middleware.ServeHTTP(w, req)

	assert.Equal(t, w.Code, http.StatusOK, "Response status should be 200")
	assert.Equal(t, w.Body.String(), "test response", "Response body should match")

	// Check Content-Type header includes charset
	expectedContentType := "application/rss+xml; charset=utf-8"
	assert.Equal(t, w.Header().Get("Content-Type"), expectedContentType,
		"Content-Type header should include charset")

	cacheKey := middleware.GetCacheKey(params)
	cachePath := filepath.Join(middleware.TmpDir, cacheKey)

	// Verify cache file exists
	_, err = os.Stat(cachePath)
	assert.NilError(t, err, "Cache file should exist after first request")

	// Test serving from cache
	w2 := httptest.NewRecorder()
	middleware.ServeHTTP(w2, req)

	assert.Equal(t, w2.Code, http.StatusOK, "Cached response status should be 200")
	assert.Assert(t, w2.Body.String() == "test response",
		"Cached response should contain expected content")
	assert.Equal(t, w2.Header().Get("Content-Type"), expectedContentType,
		"Cached response should have correct Content-Type")

	// Cleanup
	t.Cleanup(func() {
		os.Remove(cachePath)
	})
}

func TestGetCacheKey(t *testing.T) {
	t.Parallel()

	middleware := &CacheMiddleware{}

	params1 := url.Values{}
	params1.Set("url", "https://example.com/feed")
	params1.Set("title.contains", "test")

	params2 := url.Values{}
	params2.Set("url", "https://example.com/feed")
	params2.Set("title.contains", "different")

	key1 := middleware.GetCacheKey(params1)
	key2 := middleware.GetCacheKey(params2)

	assert.Assert(t, key1 != key2, "Different parameters should generate different cache keys")
	assert.Assert(t, key1 != "", "Cache key should not be empty")
	assert.Assert(t, key2 != "", "Cache key should not be empty")

	// Verify cache key format
	assert.Equal(t, filepath.Ext(key1), ".rss", "Cache key should have .rss extension")
	assert.Equal(t, filepath.Ext(key2), ".rss", "Cache key should have .rss extension")
}

func TestIsCacheFresh(t *testing.T) {
	t.Parallel()

	middleware := &CacheMiddleware{}

	// Set up times for testing (use UTC to avoid timezone issues)
	now := time.Now().UTC()
	cacheTime := now.Add(-1 * time.Hour)      // Cache was created 1 hour ago
	oldContentTime := now.Add(-2 * time.Hour) // Content is 2 hours old (older than cache)
	newContentTime := now                     // Content is fresh (newer than cache)

	t.Run("cache_fresh_when_last_modified_older", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Last-Modified", oldContentTime.Format(http.TimeFormat))
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer server.Close()

		fresh := middleware.IsCacheFresh(context.Background(), server.URL, "test-key", cacheTime)
		assert.Assert(t, fresh, "Cache should be fresh when Last-Modified is older than cache time")
	})

	t.Run("cache_stale_when_last_modified_newer", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Last-Modified", newContentTime.Format(http.TimeFormat))
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer server.Close()

		fresh := middleware.IsCacheFresh(context.Background(), server.URL, "test-key", cacheTime)
		assert.Assert(t, !fresh, "Cache should be stale when Last-Modified is newer than cache time")
	})
}

func TestCacheETagStorage(t *testing.T) {
	t.Parallel()

	middleware, err := NewCacheMiddleware(nil)
	assert.NilError(t, err, "Failed to create cache middleware")

	cacheKey := testETagKey
	testETag := testETagValue

	// Test storing and retrieving ETag
	middleware.StoreETag(cacheKey, testETag)
	retrievedETag := middleware.GetStoredETag(cacheKey)
	assert.Equal(t, retrievedETag, testETag, "Retrieved ETag should match stored ETag")

	// Test removing ETag
	middleware.RemoveETag(cacheKey)
	removedETag := middleware.GetStoredETag(cacheKey)
	assert.Equal(t, removedETag, "", "ETag should be empty after removal")
}

func TestCacheETagValidation(t *testing.T) {
	t.Parallel()

	middleware, err := NewCacheMiddleware(nil)
	assert.NilError(t, err, "Failed to create cache middleware")

	t.Run("cache_fresh_when_etag_matches", func(t *testing.T) {
		t.Parallel()
		testETagMatches(t, middleware)
	})

	t.Run("cache_stale_when_etag_differs", func(t *testing.T) {
		t.Parallel()
		testETagDiffers(t, middleware)
	})

	t.Run("cache_fresh_on_304_not_modified", func(t *testing.T) {
		t.Parallel()
		testETag304NotModified(t, middleware)
	})
}

func testETagMatches(t *testing.T, middleware *CacheMiddleware) {
	t.Helper()

	cacheKey := testETagKey
	cacheTime := time.Now().UTC().Add(-1 * time.Hour)
	testETag := testETagValue

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("ETag", testETag)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	middleware.StoreETag(cacheKey, testETag)
	defer middleware.RemoveETag(cacheKey)

	fresh := middleware.IsCacheFresh(context.Background(), server.URL, cacheKey, cacheTime)
	assert.Assert(t, fresh, "Cache should be fresh when ETag matches")
}

func testETagDiffers(t *testing.T, middleware *CacheMiddleware) {
	t.Helper()

	cacheKey := testETagKey
	cacheTime := time.Now().UTC().Add(-1 * time.Hour)
	testETag := testETagValue

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("ETag", `"xyz789"`)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	middleware.StoreETag(cacheKey, testETag)
	defer middleware.RemoveETag(cacheKey)

	fresh := middleware.IsCacheFresh(context.Background(), server.URL, cacheKey, cacheTime)
	assert.Assert(t, !fresh, "Cache should be stale when ETag is different")
}

func testETag304NotModified(t *testing.T, middleware *CacheMiddleware) {
	t.Helper()

	cacheKey := testETagKey
	cacheTime := time.Now().UTC().Add(-1 * time.Hour)
	testETag := testETagValue

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			if r.Header.Get("If-None-Match") == testETag {
				w.WriteHeader(http.StatusNotModified)
			} else {
				w.Header().Set("ETag", testETag)
				w.WriteHeader(http.StatusOK)
			}
		}
	}))
	defer server.Close()

	middleware.StoreETag(cacheKey, testETag)
	defer middleware.RemoveETag(cacheKey)

	fresh := middleware.IsCacheFresh(context.Background(), server.URL, cacheKey, cacheTime)
	assert.Assert(t, fresh, "Cache should be fresh when server returns 304 Not Modified")
}

func TestCacheMiddlewareWithTempDir(t *testing.T) {
	t.Parallel()

	// Create a temporary directory for testing
	tmpDir := fs.NewDir(t, "cache-test")
	defer tmpDir.Remove()

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("temp dir test"))
	})

	middleware, err := NewCacheMiddleware(testHandler)
	assert.NilError(t, err, "Failed to create cache middleware")

	params := url.Values{}
	params.Set("url", "https://example.com/test")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/?"+params.Encode(), nil)
	w := httptest.NewRecorder()

	middleware.ServeHTTP(w, req)

	assert.Equal(t, w.Code, http.StatusOK, "Response status should be 200")
	assert.Equal(t, w.Body.String(), "temp dir test", "Response body should match")

	// Verify cache file was created
	cacheKey := middleware.GetCacheKey(params)
	cachePath := filepath.Join(middleware.TmpDir, cacheKey)

	_, err = os.Stat(cachePath)
	assert.NilError(t, err, "Cache file should exist")

	// Cleanup
	t.Cleanup(func() {
		os.Remove(cachePath)
	})
}

func TestCacheMiddlewareErrorHandling(t *testing.T) {
	t.Parallel()

	// Test with handler that returns no content
	emptyHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware, err := NewCacheMiddleware(emptyHandler)
	assert.NilError(t, err, "Failed to create cache middleware")

	params := url.Values{}
	params.Set("url", "https://example.com/empty")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/?"+params.Encode(), nil)
	w := httptest.NewRecorder()

	middleware.ServeHTTP(w, req)

	// Should return error due to empty response body
	assert.Equal(t, w.Code, http.StatusInternalServerError,
		"Should return 500 for empty response body")
}
