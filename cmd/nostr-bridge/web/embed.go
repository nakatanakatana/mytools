package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/*
var assets embed.FS

// Handler serves the embedded nostr-bridge web frontend.
func Handler() http.Handler {
	root, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(root))
}
