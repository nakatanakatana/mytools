package main

import (
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
	"github.com/nakatanakatana/mytools/internal/webpush"
)

const (
	maxPushBodyBytes     = 16 * 1024
	maxPushEndpointLen   = 2048
	maxPushSubscriptions = 100
)

type pushAPIHandler struct {
	store             bridgestore.NotificationStore
	getVAPIDPublicKey func() string
}

// RegisterPushRoutes attaches the push notification endpoints to mux.
func RegisterPushRoutes(mux *http.ServeMux, store bridgestore.NotificationStore, getVAPIDPublicKey func() string) {
	h := &pushAPIHandler{
		store:             store,
		getVAPIDPublicKey: getVAPIDPublicKey,
	}
	mux.HandleFunc("GET /api/push/vapid-public-key", h.handleVAPIDPublicKey)
	mux.HandleFunc("POST /api/push/subscribe", h.handleSubscribe)
	mux.HandleFunc("POST /api/push/unsubscribe", h.handleUnsubscribe)
}

func (h *pushAPIHandler) handleVAPIDPublicKey(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var key string
	if h.getVAPIDPublicKey != nil {
		key = h.getVAPIDPublicKey()
	}
	if key == "" {
		writePushError(w, http.StatusServiceUnavailable, "VAPID public key not available")
		return
	}
	writePushJSON(w, http.StatusOK, map[string]string{
		"public_key": key,
	})
}

type pushSubscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (h *pushAPIHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePushJSON[pushSubscribeRequest](w, r)
	if !ok {
		return
	}

	endpoint := strings.TrimSpace(req.Endpoint)
	if endpoint == "" {
		writePushError(w, http.StatusBadRequest, "missing endpoint")
		return
	}

	if !validateEndpoint(endpoint) {
		writePushError(w, http.StatusBadRequest, "endpoint must be a valid https URL")
		return
	}

	p256dh := strings.TrimSpace(req.Keys.P256dh)
	auth := strings.TrimSpace(req.Keys.Auth)
	if p256dh == "" || auth == "" {
		writePushError(w, http.StatusBadRequest, "missing subscription keys")
		return
	}

	p256dhBytes, err := webpush.DecodeBase64Flex(p256dh)
	if err != nil || len(p256dhBytes) != 65 {
		writePushError(w, http.StatusBadRequest, "invalid p256dh key: must be valid base64-encoded 65-byte public key")
		return
	}

	if _, err := ecdh.P256().NewPublicKey(p256dhBytes); err != nil {
		writePushError(w, http.StatusBadRequest, "invalid p256dh key: point is not on curve")
		return
	}

	authBytes, err := webpush.DecodeBase64Flex(auth)
	if err != nil || len(authBytes) != 16 {
		writePushError(w, http.StatusBadRequest, "invalid auth secret: must be valid base64-encoded and exactly 16 bytes")
		return
	}

	now := time.Now().UTC()
	sub := bridgestore.StoredSubscription{
		Endpoint:  endpoint,
		P256dh:    p256dh,
		Auth:      auth,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := h.store.UpsertSubscriptionWithLimit(r.Context(), sub, maxPushSubscriptions); err != nil {
		if errors.Is(err, bridgestore.ErrMaxSubscriptionsReached) {
			writePushError(w, http.StatusTooManyRequests, "maximum subscriptions reached")
			return
		}
		writePushError(w, http.StatusInternalServerError, "failed to save subscription")
		return
	}

	writePushJSON(w, http.StatusOK, map[string]string{
		"status": "subscribed",
	})
}

type pushUnsubscribeRequest struct {
	Endpoint string `json:"endpoint"`
}

func (h *pushAPIHandler) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePushJSON[pushUnsubscribeRequest](w, r)
	if !ok {
		return
	}

	endpoint := strings.TrimSpace(req.Endpoint)
	if endpoint == "" {
		writePushError(w, http.StatusBadRequest, "missing endpoint")
		return
	}

	if !validateEndpoint(endpoint) {
		writePushError(w, http.StatusBadRequest, "endpoint must be a valid https URL")
		return
	}

	if err := h.store.DeleteSubscription(r.Context(), endpoint); err != nil {
		writePushError(w, http.StatusInternalServerError, "failed to delete subscription")
		return
	}

	writePushJSON(w, http.StatusOK, map[string]string{
		"status": "unsubscribed",
	})
}

func validateJSONContentType(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(ct)
	return err == nil && mediaType == "application/json"
}

func validateEndpoint(raw string) bool {
	if raw == "" || len(raw) > maxPushEndpointLen {
		return false
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Host == "" || u.Scheme != "https" {
		return false
	}
	// Reject userinfo credentials in push endpoints (e.g. https://user:pass@host/)
	if u.User != nil {
		return false
	}
	// Web Push endpoints must use HTTPS standard port 443
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && webpush.IsDisallowedIP(ip) {
		return false
	}
	return true
}

func writePushJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writePushError(w http.ResponseWriter, status int, msg string) {
	writePushJSON(w, status, map[string]string{"error": msg})
}

func decodePushJSON[T any](w http.ResponseWriter, r *http.Request) (*T, bool) {
	if !validateJSONContentType(r) {
		writePushError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return nil, false
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPushBodyBytes)

	var req T
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writePushError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return nil, false
		}
		writePushError(w, http.StatusBadRequest, "invalid JSON")
		return nil, false
	}
	return &req, true
}

