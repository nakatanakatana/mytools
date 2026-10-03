package main

import (
	"net/http"
)

type JSONProvider interface {
	GetJSON(name string) []byte
}

type NIP05Handler struct {
	provider JSONProvider
}

func NewNIP05Handler(provider JSONProvider) *NIP05Handler {
	return &NIP05Handler{
		provider: provider,
	}
}

func (h *NIP05Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	// name is empty string if not present, which maps to full list in our logic

	data := h.provider.GetJSON(name)
	if data == nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}