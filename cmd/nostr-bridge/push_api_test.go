package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

func generateTestSubscriptionKeys(t *testing.T) (string, string) {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	pub := base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes())
	auth := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef")) // 16 bytes
	return pub, auth
}

type mockNotificationStore struct {
	mu            sync.Mutex
	vapidKeys     *bridgestore.StoredVAPIDKeys
	subscriptions map[string]bridgestore.StoredSubscription
	upsertErr     error
	deleteErr     error
	getVAPIDErr   error
	saveVAPIDErr  error
}

func newMockNotificationStore() *mockNotificationStore {
	return &mockNotificationStore{
		subscriptions: make(map[string]bridgestore.StoredSubscription),
	}
}

func (m *mockNotificationStore) GetVAPIDKeys(_ context.Context) (*bridgestore.StoredVAPIDKeys, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getVAPIDErr != nil {
		return nil, m.getVAPIDErr
	}
	if m.vapidKeys == nil {
		return nil, nil
	}
	cp := *m.vapidKeys
	return &cp, nil
}

func (m *mockNotificationStore) SaveVAPIDKeys(_ context.Context, keys bridgestore.StoredVAPIDKeys) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveVAPIDErr != nil {
		return m.saveVAPIDErr
	}
	m.vapidKeys = &keys
	return nil
}

func (m *mockNotificationStore) SaveVAPIDKeysIfAbsent(ctx context.Context, keys bridgestore.StoredVAPIDKeys) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveVAPIDErr != nil {
		return m.saveVAPIDErr
	}
	if m.vapidKeys == nil {
		m.vapidKeys = &keys
	}
	return nil
}


func (m *mockNotificationStore) UpsertSubscriptionWithLimit(_ context.Context, sub bridgestore.StoredSubscription, maxLimit int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.upsertErr != nil {
		return m.upsertErr
	}
	if maxLimit > 0 {
		if _, exists := m.subscriptions[sub.Endpoint]; !exists && len(m.subscriptions) >= maxLimit {
			return bridgestore.ErrMaxSubscriptionsReached
		}
	}
	m.subscriptions[sub.Endpoint] = sub
	return nil
}

func (m *mockNotificationStore) DeleteSubscription(_ context.Context, endpoint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.subscriptions, endpoint)
	return nil
}

func (m *mockNotificationStore) ListSubscriptions(_ context.Context) ([]bridgestore.StoredSubscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	subs := make([]bridgestore.StoredSubscription, 0, len(m.subscriptions))
	for _, sub := range m.subscriptions {
		subs = append(subs, sub)
	}
	return subs, nil
}

func TestPushAPI_VAPIDPublicKey(t *testing.T) {
	expectedKey := "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkjCMqVzpU3ODAOM8"
	store := newMockNotificationStore()
	mux := http.NewServeMux()
	RegisterPushRoutes(mux, store, func() string {
		return expectedKey
	})

	req := httptest.NewRequest(http.MethodGet, "/api/push/vapid-public-key", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("expected Cache-Control 'no-store', got %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", got)
	}

	var resp struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode JSON response: %v", err)
	}
	if resp.PublicKey != expectedKey {
		t.Errorf("expected public_key %q, got %q", expectedKey, resp.PublicKey)
	}
}

func TestPushAPI_Subscribe_Success(t *testing.T) {
	store := newMockNotificationStore()
	mux := http.NewServeMux()
	RegisterPushRoutes(mux, store, func() string { return "test-key" })

	testPub, testAuth := generateTestSubscriptionKeys(t)
	body := map[string]any{
		"endpoint": "https://push.example.com/subscription/123",
		"keys": map[string]string{
			"p256dh": testPub,
			"auth":   testAuth,
		},
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/push/subscribe", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", got)
	}

	var resp struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode JSON response: %v", err)
	}
	if resp.Status != "subscribed" {
		t.Errorf("expected status 'subscribed', got %q", resp.Status)
	}

	store.mu.Lock()
	sub, exists := store.subscriptions["https://push.example.com/subscription/123"]
	store.mu.Unlock()
	if !exists {
		t.Fatal("expected subscription to be saved in store")
	}
	if sub.P256dh != testPub {
		t.Errorf("unexpected p256dh in store: %q", sub.P256dh)
	}
	if sub.Auth != testAuth {
		t.Errorf("unexpected auth in store: %q", sub.Auth)
	}
}

func TestPushAPI_Subscribe_ValidationErrors(t *testing.T) {
	validP256dh, validAuth := generateTestSubscriptionKeys(t)

	tests := []struct {
		name        string
		body        string
		errContains string
	}{
		{
			name:        "invalid JSON",
			body:        `{invalid json`,
			errContains: "invalid",
		},
		{
			name:        "missing endpoint",
			body:        `{"keys":{"p256dh":"` + validP256dh + `","auth":"` + validAuth + `"}}`,
			errContains: "endpoint",
		},
		{
			name:        "empty endpoint",
			body:        `{"endpoint":"","keys":{"p256dh":"` + validP256dh + `","auth":"` + validAuth + `"}}`,
			errContains: "endpoint",
		},
		{
			name:        "invalid URL scheme ftp",
			body:        `{"endpoint":"ftp://push.example.com","keys":{"p256dh":"` + validP256dh + `","auth":"` + validAuth + `"}}`,
			errContains: "endpoint",
		},
		{
			name:        "invalid URL relative path",
			body:        `{"endpoint":"/api/push","keys":{"p256dh":"` + validP256dh + `","auth":"` + validAuth + `"}}`,
			errContains: "endpoint",
		},
		{
			name:        "missing keys object",
			body:        `{"endpoint":"https://push.example.com"}`,
			errContains: "keys",
		},
		{
			name:        "missing p256dh key",
			body:        `{"endpoint":"https://push.example.com","keys":{"auth":"` + validAuth + `"}}`,
			errContains: "keys",
		},
		{
			name:        "empty p256dh key",
			body:        `{"endpoint":"https://push.example.com","keys":{"p256dh":"","auth":"` + validAuth + `"}}`,
			errContains: "keys",
		},
		{
			name:        "missing auth key",
			body:        `{"endpoint":"https://push.example.com","keys":{"p256dh":"` + validP256dh + `"}}`,
			errContains: "keys",
		},
		{
			name:        "empty auth key",
			body:        `{"endpoint":"https://push.example.com","keys":{"p256dh":"` + validP256dh + `","auth":""}}`,
			errContains: "keys",
		},
		{
			name:        "invalid base64 in p256dh",
			body:        `{"endpoint":"https://push.example.com","keys":{"p256dh":"not-valid-base64!@#$","auth":"` + validAuth + `"}}`,
			errContains: "base64",
		},
		{
			name:        "invalid base64 in auth",
			body:        `{"endpoint":"https://push.example.com","keys":{"p256dh":"` + validP256dh + `","auth":"not-valid-base64!@#$"}}`,
			errContains: "base64",
		},
		{
			name:        "short p256dh key (not 65 bytes)",
			body:        `{"endpoint":"https://push.example.com","keys":{"p256dh":"` + base64.RawURLEncoding.EncodeToString(make([]byte, 64)) + `","auth":"` + validAuth + `"}}`,
			errContains: "p256dh",
		},
		{
			name:        "long p256dh key (not 65 bytes)",
			body:        `{"endpoint":"https://push.example.com","keys":{"p256dh":"` + base64.RawURLEncoding.EncodeToString(make([]byte, 66)) + `","auth":"` + validAuth + `"}}`,
			errContains: "p256dh",
		},
		{
			name:        "short auth key (less than 16 bytes)",
			body:        `{"endpoint":"https://push.example.com","keys":{"p256dh":"` + validP256dh + `","auth":"` + base64.RawURLEncoding.EncodeToString(make([]byte, 15)) + `"}}`,
			errContains: "auth",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockNotificationStore()
			mux := http.NewServeMux()
			RegisterPushRoutes(mux, store, func() string { return "test-key" })

			req := httptest.NewRequest(http.MethodPost, "/api/push/subscribe", bytes.NewBufferString(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected status 400 Bad Request, got %d: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("expected Content-Type 'application/json', got %q", got)
			}

			var resp struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode JSON error response: %v", err)
			}
			if resp.Error == "" {
				t.Error("expected non-empty error message in JSON response")
			}
			if tc.errContains != "" && !strings.Contains(strings.ToLower(resp.Error), strings.ToLower(tc.errContains)) {
				t.Errorf("expected error containing %q, got %q", tc.errContains, resp.Error)
			}
		})
	}
}

func TestPushAPI_Subscribe_StoreError(t *testing.T) {
	store := newMockNotificationStore()
	store.upsertErr = errors.New("db error")
	mux := http.NewServeMux()
	RegisterPushRoutes(mux, store, func() string { return "test-key" })

	pub, auth := generateTestSubscriptionKeys(t)
	body := fmt.Sprintf(`{"endpoint":"https://push.example.com","keys":{"p256dh":%q,"auth":%q}}`, pub, auth)
	req := httptest.NewRequest(http.MethodPost, "/api/push/subscribe", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPushAPI_Unsubscribe_Success(t *testing.T) {
	store := newMockNotificationStore()
	store.subscriptions["https://push.example.com/sub/456"] = bridgestore.StoredSubscription{
		Endpoint: "https://push.example.com/sub/456",
	}

	mux := http.NewServeMux()
	RegisterPushRoutes(mux, store, func() string { return "test-key" })

	body := `{"endpoint":"https://push.example.com/sub/456"}`
	req := httptest.NewRequest(http.MethodPost, "/api/push/unsubscribe", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", got)
	}

	var resp struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode JSON response: %v", err)
	}
	if resp.Status != "unsubscribed" {
		t.Errorf("expected status 'unsubscribed', got %q", resp.Status)
	}

	store.mu.Lock()
	_, exists := store.subscriptions["https://push.example.com/sub/456"]
	store.mu.Unlock()
	if exists {
		t.Fatal("expected subscription to be deleted from store")
	}
}

func TestPushAPI_Unsubscribe_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "invalid JSON",
			body: `{invalid json`,
		},
		{
			name: "missing endpoint",
			body: `{}`,
		},
		{
			name: "empty endpoint",
			body: `{"endpoint":""}`,
		},
		{
			name: "whitespace endpoint",
			body: `{"endpoint":"   "}`,
		},
		{
			name: "invalid URL scheme ftp",
			body: `{"endpoint":"ftp://push.example.com"}`,
		},
		{
			name: "invalid URL relative path",
			body: `{"endpoint":"/api/push"}`,
		},
		{
			name: "invalid URL not-a-url",
			body: `{"endpoint":"://not-a-url"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockNotificationStore()
			mux := http.NewServeMux()
			RegisterPushRoutes(mux, store, func() string { return "test-key" })

			req := httptest.NewRequest(http.MethodPost, "/api/push/unsubscribe", bytes.NewBufferString(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected status 400 Bad Request, got %d: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("expected Content-Type 'application/json', got %q", got)
			}

			var resp struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode JSON error response: %v", err)
			}
			if resp.Error == "" {
				t.Error("expected non-empty error message in JSON response")
			}
		})
	}
}

func TestPushAPI_Unsubscribe_StoreError(t *testing.T) {
	store := newMockNotificationStore()
	store.deleteErr = errors.New("db error")
	mux := http.NewServeMux()
	RegisterPushRoutes(mux, store, func() string { return "test-key" })

	body := `{"endpoint":"https://push.example.com/sub/456"}`
	req := httptest.NewRequest(http.MethodPost, "/api/push/unsubscribe", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPushAPI_ContentTypeValidation(t *testing.T) {
	store := newMockNotificationStore()
	mux := http.NewServeMux()
	RegisterPushRoutes(mux, store, func() string { return "test-key" })

	endpoints := []string{"/api/push/subscribe", "/api/push/unsubscribe"}
	contentTypes := []string{"", "text/plain", "application/xml", "multipart/form-data"}

	for _, ep := range endpoints {
		for _, ct := range contentTypes {
			t.Run(ep+"_ct_"+ct, func(t *testing.T) {
				body := `{"endpoint":"https://push.example.com"}`
				req := httptest.NewRequest(http.MethodPost, ep, bytes.NewBufferString(body))
				if ct != "" {
					req.Header.Set("Content-Type", ct)
				}
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)

				if rec.Code != http.StatusUnsupportedMediaType {
					t.Fatalf("expected status 415 Unsupported Media Type, got %d: %s", rec.Code, rec.Body.String())
				}
				if got := rec.Header().Get("Content-Type"); got != "application/json" {
					t.Errorf("expected Content-Type 'application/json', got %q", got)
				}
				var resp struct {
					Error string `json:"error"`
				}
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("decode JSON error response: %v", err)
				}
				if resp.Error == "" {
					t.Error("expected non-empty error message in JSON response")
				}
			})
		}
	}
}

func TestPushAPI_BodySizeLimit(t *testing.T) {
	store := newMockNotificationStore()
	mux := http.NewServeMux()
	RegisterPushRoutes(mux, store, func() string { return "test-key" })

	// Create body exceeding 16KB limit
	largePayload := make([]byte, 17*1024)
	for i := range largePayload {
		largePayload[i] = 'a'
	}

	endpoints := []string{"/api/push/subscribe", "/api/push/unsubscribe"}
	for _, ep := range endpoints {
		t.Run(ep+"_exceeds_limit", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, ep, bytes.NewReader(largePayload))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
				t.Fatalf("expected status 413 or 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("expected Content-Type 'application/json', got %q", got)
			}
		})
	}
}

func TestPushAPI_VAPIDPublicKey_NotAvailable(t *testing.T) {
	store := newMockNotificationStore()
	mux := http.NewServeMux()
	RegisterPushRoutes(mux, store, func() string { return "" })

	req := httptest.NewRequest(http.MethodGet, "/api/push/vapid-public-key", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPushAPI_Subscribe_Hardening(t *testing.T) {
	store := newMockNotificationStore()
	mux := http.NewServeMux()
	RegisterPushRoutes(mux, store, func() string { return "test-key" })

	validP256dh, validAuth := generateTestSubscriptionKeys(t)

	t.Run("rejects_http_and_private_ips", func(t *testing.T) {
		badEndpoints := []string{
			"http://push.example.com/sub",
			"https://127.0.0.1/sub",
			"https://localhost/sub",
			"https://0.0.0.0/sub",
			"https://100.64.0.1/sub",
			"https://224.0.0.1/sub",
			"https://169.254.169.254/sub",
			"https://10.0.0.1/sub",
			"https://192.168.1.1/sub",
			"https://[::1]/sub",
			"https://[ff02::1]/sub",
			"https://user:pass@push.example.com/sub",
			"https://push.example.com:8080/sub",
			"https://push.example.com:8443/sub",
		}
		for _, ep := range badEndpoints {
			body, _ := json.Marshal(map[string]any{
				"endpoint": ep,
				"keys": map[string]string{
					"p256dh": validP256dh,
					"auth":   validAuth,
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/api/push/subscribe", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("endpoint %q: expected status 400, got %d", ep, rec.Code)
			}
		}
	})

	t.Run("rejects_invalid_p256dh_curve_point", func(t *testing.T) {
		// 65 bytes starting with 0x04 but not a valid point on P-256
		fakePoint := make([]byte, 65)
		fakePoint[0] = 0x04
		badP256dh := base64.RawURLEncoding.EncodeToString(fakePoint)

		body, _ := json.Marshal(map[string]any{
			"endpoint": "https://push.example.com/sub",
			"keys": map[string]string{
				"p256dh": badP256dh,
				"auth":   validAuth,
			},
		})
		req := httptest.NewRequest(http.MethodPost, "/api/push/subscribe", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected status 400 for invalid curve point, got %d", rec.Code)
		}
	})

	t.Run("rejects_auth_secret_not_exact_16_bytes", func(t *testing.T) {
		badAuths := []string{
			base64.RawURLEncoding.EncodeToString(make([]byte, 15)), // 15 bytes
			base64.RawURLEncoding.EncodeToString(make([]byte, 17)), // 17 bytes
		}
		for _, badAuth := range badAuths {
			body, _ := json.Marshal(map[string]any{
				"endpoint": "https://push.example.com/sub",
				"keys": map[string]string{
					"p256dh": validP256dh,
					"auth":   badAuth,
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/api/push/subscribe", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("expected status 400 for non-16-byte auth, got %d", rec.Code)
			}
		}
	})
}
