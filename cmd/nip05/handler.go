package main

import (
	"net/http"
)

type FilePathProvider interface {
	GetFilePath(name string) string
}

type NIP05Handler struct {
	provider FilePathProvider
}

func NewNIP05Handler(provider FilePathProvider) *NIP05Handler {
	return &NIP05Handler{
		provider: provider,
	}
}

func (h *NIP05Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	// name is empty string if not present, which maps to full list in our logic

	filePath := h.provider.GetFilePath(name)
	if filePath == "" {
		// File not found for this name (or unknown user)
		// Assuming unknown user should return 404 or empty JSON?
		// Existing behavior for unknown was to return empty maps.
		// If we want to strictly follow that, we need a "empty.json" generated.
		// However, for now let's return 404 as it is cleaner for static file server.
		// If spec requires 200 OK with empty body, we need to generate that file.
		// Let's stick to 404 for missing file.
		http.NotFound(w, r)
		return
	}

	// Content-Type header is usually sniffed by ServeFile, 
	// but for JSON it's good practice to set it explicitly if extension is correct.
	// Since our files are .json, ServeFile should handle it. 
	
	http.ServeFile(w, r, filePath)
}