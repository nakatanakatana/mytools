package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	httpMethodHead = "HEAD"
	requestTimeout = 10 * time.Second
	filePerms      = 0o600
	dirPerms       = 0o755
)

var ErrNoResponseBody = errors.New("no response body to cache")

type CacheMiddleware struct {
	TmpDir    string
	next      http.Handler
	etags     map[string]string
	etagMutex sync.RWMutex
	fsys      fs.FS
}

func NewCacheMiddleware(next http.Handler) (*CacheMiddleware, error) {
	tmpDir := os.TempDir()
	cacheDir := filepath.Join(tmpDir, "ff-cache")

	if err := os.MkdirAll(cacheDir, dirPerms); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	return &CacheMiddleware{
		TmpDir: cacheDir,
		next:   next,
		etags:  make(map[string]string),
		fsys:   os.DirFS(cacheDir),
	}, nil
}

func (c *CacheMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	queries := r.URL.Query()
	cacheKey := c.GetCacheKey(queries)
	cachePath := filepath.Join(c.TmpDir, cacheKey)

	// Check if cache file exists
	stat, err := os.Stat(cachePath)
	if err != nil {
		// Cache miss - generate new response
		c.generateAndCacheResponse(w, r, queries, cachePath, cacheKey)

		return
	}

	// Cache file exists - check if we need freshness validation
	upstreamURLs := queries["url"]
	if len(upstreamURLs) == 0 {
		// No URL parameter - serve cached file directly
		c.serveFileWithCharset(w, r, cacheKey)

		return
	}

	// Check if cache is fresh
	if !c.IsCacheFresh(r.Context(), upstreamURLs[0], cacheKey, stat.ModTime()) {
		// Cache is stale - remove and regenerate
		os.Remove(cachePath)
		c.RemoveETag(cacheKey)
		c.generateAndCacheResponse(w, r, queries, cachePath, cacheKey)

		return
	}

	// Cache is fresh - serve it
	c.serveFileWithCharset(w, r, cacheKey)
}

func (c *CacheMiddleware) generateAndCacheResponse(
	w http.ResponseWriter, r *http.Request, queries url.Values, cachePath, cacheKey string,
) {
	responseRecorder := &ResponseRecorder{
		ResponseWriter:  w,
		cachePath:       cachePath,
		cacheMiddleware: c,
		cacheKey:        cacheKey,
		statusCode:      http.StatusOK,
	}

	c.next.ServeHTTP(responseRecorder, r)

	upstreamURLs := queries["url"]
	if len(upstreamURLs) > 0 {
		responseRecorder.captureAndStoreETag(r.Context(), upstreamURLs[0])
	}

	// Write response to cache file and serve from filesystem
	if err := responseRecorder.writeToCache(); err != nil {
		http.Error(w, "Failed to cache response", http.StatusInternalServerError)

		return
	}

	// Serve the cached file
	c.serveFileWithCharset(w, r, cacheKey)
}

func (c *CacheMiddleware) GetCacheKey(params url.Values) string {
	h := sha256.New()
	h.Write([]byte(params.Encode()))

	return fmt.Sprintf("%x.rss", h.Sum(nil))
}

func (c *CacheMiddleware) serveFileWithCharset(w http.ResponseWriter, r *http.Request, filename string) {
	// Set Content-Type with charset before serving the file
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	http.ServeFileFS(w, r, c.fsys, filename)
}

type ResponseRecorder struct {
	http.ResponseWriter
	cachePath       string
	cacheMiddleware *CacheMiddleware
	cacheKey        string
	body            []byte
	statusCode      int
}

func (c *CacheMiddleware) IsCacheFresh(
	ctx context.Context, upstreamURL string, cacheKey string, cacheTime time.Time,
) bool {
	req, err := http.NewRequestWithContext(ctx, httpMethodHead, upstreamURL, nil) // #nosec G704
	if err != nil {
		return true
	}

	storedETag := c.GetStoredETag(cacheKey)
	if storedETag != "" {
		req.Header.Set("If-None-Match", storedETag)
	}

	client := &http.Client{
		Timeout: requestTimeout,
	}

	resp, err := client.Do(req) // #nosec G704
	if err != nil {
		return true
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return true
	}

	currentETag := resp.Header.Get("ETag")
	if currentETag != "" && storedETag != "" {
		return currentETag == storedETag
	}

	if lastModified := resp.Header.Get("Last-Modified"); lastModified != "" {
		if lastModTime, err := http.ParseTime(lastModified); err == nil {
			cacheTimeUTC := cacheTime.UTC()

			return !lastModTime.After(cacheTimeUTC)
		}
	}

	return false
}

func (c *CacheMiddleware) GetStoredETag(cacheKey string) string {
	c.etagMutex.RLock()
	defer c.etagMutex.RUnlock()

	return c.etags[cacheKey]
}

func (c *CacheMiddleware) StoreETag(cacheKey string, etag string) {
	if etag == "" {
		return
	}

	c.etagMutex.Lock()
	defer c.etagMutex.Unlock()

	c.etags[cacheKey] = etag
}

func (c *CacheMiddleware) RemoveETag(cacheKey string) {
	c.etagMutex.Lock()
	defer c.etagMutex.Unlock()

	delete(c.etags, cacheKey)
}

func (r *ResponseRecorder) Write(data []byte) (int, error) {
	r.body = append(r.body, data...)

	return len(data), nil
}

func (r *ResponseRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
}

func (r *ResponseRecorder) writeToCache() error {
	if len(r.body) == 0 {
		return ErrNoResponseBody
	}

	if err := os.WriteFile(r.cachePath, r.body, filePerms); err != nil {
		return fmt.Errorf("failed to write cache file: %w", err)
	}

	return nil
}

func (r *ResponseRecorder) captureAndStoreETag(ctx context.Context, upstreamURL string) {
	req, err := http.NewRequestWithContext(ctx, httpMethodHead, upstreamURL, nil) // #nosec G704
	if err != nil {
		return
	}

	client := &http.Client{
		Timeout: requestTimeout,
	}

	resp, err := client.Do(req) // #nosec G704
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if etag := resp.Header.Get("ETag"); etag != "" {
		r.cacheMiddleware.StoreETag(r.cacheKey, etag)
	}
}
