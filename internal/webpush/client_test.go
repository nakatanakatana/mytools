package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func setupTestSubscription(t *testing.T, endpoint string) (Subscription, *ecdh.PrivateKey, []byte) {
	t.Helper()

	clientPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}

	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatalf("generate auth secret: %v", err)
	}

	sub := Subscription{
		Endpoint: endpoint,
		Keys: SubscriptionKeys{
			P256dh: base64.RawURLEncoding.EncodeToString(clientPriv.PublicKey().Bytes()),
			Auth:   base64.RawURLEncoding.EncodeToString(authSecret),
		},
	}

	return sub, clientPriv, authSecret
}

func TestSend_Success(t *testing.T) {
	statusCodes := []int{
		http.StatusOK,       // 200
		http.StatusCreated,  // 201
		http.StatusAccepted, // 202
	}

	for _, statusCode := range statusCodes {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			vapidKeys, err := GenerateVAPIDKeys()
			if err != nil {
				t.Fatalf("GenerateVAPIDKeys: %v", err)
			}

			payload := []byte(`{"title":"Nostr Event","body":"You received a notification"}`)

			var receivedReq *http.Request
			var receivedBody []byte

			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedReq = r.Clone(r.Context())
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "read error", http.StatusInternalServerError)
					return
				}
				receivedBody = body
				w.WriteHeader(statusCode)
			}))
			defer server.Close()

			sub, clientPriv, authSecret := setupTestSubscription(t, server.URL)

			client := NewClient(ClientOptions{
				HTTPClient: server.Client(),
				VAPIDKeys:  *vapidKeys,
				Subject:    "mailto:admin@example.com",
			})

			opts := Options{
				TTL:     60,
				Urgency: "high",
				Topic:   "test-topic",
			}

			ctx := context.Background()
			err = client.Send(ctx, sub, payload, opts)
			if err != nil {
				t.Fatalf("Send failed: %v", err)
			}

			if receivedReq == nil {
				t.Fatal("expected server to receive a request")
			}

			if receivedReq.Method != http.MethodPost {
				t.Errorf("Method = %q, want %q", receivedReq.Method, http.MethodPost)
			}

			if ct := receivedReq.Header.Get("Content-Type"); ct != "application/octet-stream" {
				t.Errorf("Content-Type = %q, want application/octet-stream", ct)
			}

			if ce := receivedReq.Header.Get("Content-Encoding"); ce != "aes128gcm" {
				t.Errorf("Content-Encoding = %q, want aes128gcm", ce)
			}

			if ttl := receivedReq.Header.Get("TTL"); ttl != "60" {
				t.Errorf("TTL = %q, want 60", ttl)
			}

			if urgency := receivedReq.Header.Get("Urgency"); urgency != "high" {
				t.Errorf("Urgency = %q, want high", urgency)
			}

			if topic := receivedReq.Header.Get("Topic"); topic != "test-topic" {
				t.Errorf("Topic = %q, want test-topic", topic)
			}

			authHdr := receivedReq.Header.Get("Authorization")
			if !strings.HasPrefix(authHdr, "vapid t=") {
				t.Errorf("Authorization header missing vapid token prefix: %q", authHdr)
			}
			if !strings.Contains(authHdr, ", k="+vapidKeys.PublicKey) {
				t.Errorf("Authorization header missing public key %q: %q", vapidKeys.PublicKey, authHdr)
			}

			if len(receivedBody) == 0 {
				t.Fatal("expected non-empty encrypted body")
			}

			decrypted, err := DecryptForTest(receivedBody, clientPriv, authSecret)
			if err != nil {
				t.Fatalf("decrypt received body: %v", err)
			}
			if string(decrypted) != string(payload) {
				t.Errorf("decrypted payload = %q, want %q", string(decrypted), string(payload))
			}
		})
	}
}

func TestSend_DefaultTTLAndUrgency(t *testing.T) {
	vapidKeys, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}

	var receivedReq *http.Request
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedReq = r.Clone(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sub, _, _ := setupTestSubscription(t, server.URL)

	client := NewClient(ClientOptions{
		HTTPClient: server.Client(),
		VAPIDKeys:  *vapidKeys,
		Subject:    "mailto:admin@example.com",
	})

	// Options with default TTL (0) and default Urgency ("")
	err = client.Send(context.Background(), sub, []byte("test"), Options{})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if ttl := receivedReq.Header.Get("TTL"); ttl != "86400" {
		t.Errorf("TTL = %q, want default 86400", ttl)
	}

	if urgency := receivedReq.Header.Get("Urgency"); urgency != UrgencyHigh {
		t.Errorf("Urgency = %q, want default %q", urgency, UrgencyHigh)
	}
}

func TestSend_MissingVAPIDKeys(t *testing.T) {
	client := NewClient(ClientOptions{})
	sub, _, _ := setupTestSubscription(t, "https://example.com/push")

	err := client.Send(context.Background(), sub, []byte("test"), Options{TTL: 60})
	if err == nil {
		t.Fatal("expected error for missing VAPID keys, got nil")
	}
	if !strings.Contains(err.Error(), "vapid keys are required") {
		t.Errorf("expected vapid keys error, got: %v", err)
	}
}

func TestSend_ExpiredSubscription(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{
			name:       "404 Not Found",
			statusCode: http.StatusNotFound,
			body:       "push subscription not found",
		},
		{
			name:       "410 Gone",
			statusCode: http.StatusGone,
			body:       "push subscription expired and unsubscribed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vapidKeys, err := GenerateVAPIDKeys()
			if err != nil {
				t.Fatalf("GenerateVAPIDKeys: %v", err)
			}

			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tc.body, tc.statusCode)
			}))
			defer server.Close()

			sub, _, _ := setupTestSubscription(t, server.URL)

			client := NewClient(ClientOptions{
				HTTPClient: server.Client(),
				VAPIDKeys:  *vapidKeys,
				Subject:    "mailto:admin@example.com",
			})

			err = client.Send(context.Background(), sub, []byte("test"), Options{TTL: 60})
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			if !IsSubscriptionExpired(err) {
				t.Errorf("IsSubscriptionExpired(err) = false, want true for %d", tc.statusCode)
			}

			if !errors.Is(err, ErrSubscriptionExpired) {
				t.Errorf("errors.Is(err, ErrSubscriptionExpired) = false, want true for %d", tc.statusCode)
			}
		})
	}
}

func TestSend_ServerErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		retryAfter string
		wantRetry  time.Duration
	}{
		{
			name:       "400 Bad Request",
			statusCode: http.StatusBadRequest,
			body:       "invalid payload format",
		},
		{
			name:       "500 Internal Server Error",
			statusCode: http.StatusInternalServerError,
			body:       "push service temporary outage",
			retryAfter: "30",
			wantRetry:  30 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vapidKeys, err := GenerateVAPIDKeys()
			if err != nil {
				t.Fatalf("GenerateVAPIDKeys: %v", err)
			}

			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				http.Error(w, tc.body, tc.statusCode)
			}))
			defer server.Close()

			sub, _, _ := setupTestSubscription(t, server.URL)

			client := NewClient(ClientOptions{
				HTTPClient: server.Client(),
				VAPIDKeys:  *vapidKeys,
				Subject:    "mailto:admin@example.com",
			})

			err = client.Send(context.Background(), sub, []byte("test"), Options{TTL: 60})
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			if IsSubscriptionExpired(err) {
				t.Errorf("IsSubscriptionExpired(err) = true, want false for %d", tc.statusCode)
			}

			errMsg := err.Error()
			if strings.Contains(errMsg, strings.TrimSpace(tc.body)) {
				t.Errorf("error message %q must not contain response body %q", errMsg, tc.body)
			}
			if !strings.Contains(errMsg, fmt.Sprint(tc.statusCode)) {
				t.Errorf("error message %q should contain status code %d", errMsg, tc.statusCode)
			}
			var retryAfter interface{ RetryAfter() time.Duration }
			if !errors.As(err, &retryAfter) {
				t.Fatal("error does not expose Retry-After")
			}
			if got := retryAfter.RetryAfter(); got != tc.wantRetry {
				t.Errorf("RetryAfter() = %s, want %s", got, tc.wantRetry)
			}
		})
	}
}

func TestPushServiceErrorPermanentClassification(t *testing.T) {
	tests := []struct {
		statusCode int
		want       bool
	}{
		{statusCode: http.StatusBadRequest, want: true},
		{statusCode: http.StatusUnauthorized, want: true},
		{statusCode: http.StatusForbidden, want: true},
		{statusCode: http.StatusRequestTimeout, want: false},
		{statusCode: http.StatusTooEarly, want: false},
		{statusCode: http.StatusTooManyRequests, want: false},
		{statusCode: http.StatusInternalServerError, want: false},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.statusCode), func(t *testing.T) {
			err := &pushServiceError{statusCode: tc.statusCode}
			if got := IsPermanentPushError(err); got != tc.want {
				t.Errorf("IsPermanentPushError(%d) = %t, want %t", tc.statusCode, got, tc.want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "seconds", value: "30", want: 30 * time.Second},
		{name: "http date", value: now.Add(3 * time.Minute).Format(http.TimeFormat), want: 3 * time.Minute},
		{name: "invalid", value: "later", want: 0},
		{name: "bounded huge value", value: "2147483647", want: 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.value, now); got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = %s, want %s", tc.value, got, tc.want)
			}
		})
	}
}

func TestSend_ContextCancellation(t *testing.T) {
	vapidKeys, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sub, _, _ := setupTestSubscription(t, server.URL)

	client := NewClient(ClientOptions{
		HTTPClient: server.Client(),
		VAPIDKeys:  *vapidKeys,
		Subject:    "mailto:admin@example.com",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err = client.Send(ctx, sub, []byte("test"), Options{TTL: 60})
	if err == nil {
		t.Fatal("expected error from canceled context, got nil")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled), got: %v", err)
	}
}

func TestNewClient_DefaultTimeout(t *testing.T) {
	client := NewClient(ClientOptions{})
	if client == nil {
		t.Fatal("expected non-nil Client")
	}
	if client.httpClient == nil {
		t.Fatal("expected non-nil httpClient")
	}
	if client.httpClient.Timeout != 15*time.Second {
		t.Errorf("httpClient.Timeout = %v, want 15s", client.httpClient.Timeout)
	}
}

func TestNewClient_CustomHTTPClient(t *testing.T) {
	customHC := &http.Client{Timeout: 5 * time.Second}
	client := NewClient(ClientOptions{HTTPClient: customHC})
	if client.httpClient != customHC {
		t.Errorf("client.httpClient = %v, want %v", client.httpClient, customHC)
	}
}

func TestSend_EncryptionError(t *testing.T) {
	vapidKeys, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}

	client := NewClient(ClientOptions{
		VAPIDKeys: *vapidKeys,
		Subject:   "mailto:admin@example.com",
	})
	invalidSub := Subscription{
		Endpoint: "https://example.com/push",
		Keys: SubscriptionKeys{
			P256dh: "invalid-key",
			Auth:   "invalid-auth",
		},
	}

	err = client.Send(context.Background(), invalidSub, []byte("test"), Options{TTL: 60})
	if err == nil {
		t.Fatal("expected encryption error, got nil")
	}
	if !strings.Contains(err.Error(), "encrypt payload") {
		t.Errorf("expected encrypt payload error, got: %v", err)
	}
}

func TestSend_OptionsSanitization(t *testing.T) {
	vapidKeys, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}

	var receivedReq *http.Request
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedReq = r.Clone(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sub, _, _ := setupTestSubscription(t, server.URL)
	client := NewClient(ClientOptions{
		HTTPClient: server.Client(),
		VAPIDKeys:  *vapidKeys,
		Subject:    "mailto:admin@example.com",
	})

	opts := Options{
		TTL:     3000000,                                                        // Exceeds max TTL 2419200
		Urgency: "invalid-urgency",                                              // Invalid urgency
		Topic:   "this-is-a-very-long-topic-name-that-exceeds-thirty-two-bytes", // > 32 bytes
	}

	err = client.Send(context.Background(), sub, []byte("test"), opts)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if receivedReq == nil {
		t.Fatal("expected request")
	}

	if ttl := receivedReq.Header.Get("TTL"); ttl != "2419200" {
		t.Errorf("TTL = %q, want 2419200", ttl)
	}
	if urgency := receivedReq.Header.Get("Urgency"); urgency != "high" {
		t.Errorf("Urgency = %q, want high", urgency)
	}
	topic := receivedReq.Header.Get("Topic")
	if len(topic) > 32 {
		t.Errorf("Topic length = %d > 32: %q", len(topic), topic)
	}
	if expectedTopic := NormalizeTopic(opts.Topic); topic != expectedTopic {
		t.Errorf("Topic = %q, want %q", topic, expectedTopic)
	}
}

func TestSend_DefaultClientBlocksPrivateIP(t *testing.T) {
	vapidKeys, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sub, _, _ := setupTestSubscription(t, server.URL)

	// NewClient without opts.HTTPClient uses the default secure client
	client := NewClient(ClientOptions{
		VAPIDKeys: *vapidKeys,
		Subject:   "mailto:admin@example.com",
	})

	err = client.Send(context.Background(), sub, []byte("test"), Options{TTL: 60})
	if err == nil {
		t.Fatal("expected Send to fail against loopback/private IP with default client, got nil")
	}
	if err.Error() != "webpush: push transport failed (details redacted)" {
		t.Errorf("expected redacted transport error, got: %v", err)
	}
}

func TestNewDefaultHTTPClient_DefaultFields(t *testing.T) {
	hc := newDefaultHTTPClient()
	if hc == nil {
		t.Fatal("expected non-nil http.Client")
	}
	if hc.CheckRedirect == nil {
		t.Fatal("expected non-nil CheckRedirect")
	}
	err := hc.CheckRedirect(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "redirects are not allowed") {
		t.Errorf("expected CheckRedirect to reject, got %v", err)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", hc.Transport)
	}
	if tr.Proxy != nil {
		t.Error("expected Proxy to be nil")
	}
}

func TestIsDisallowedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.169.254", // Link-local
		"0.0.0.0",         // Current network / Linux loopback alias
		"0.1.2.3",
		"100.64.0.1",         // CGNAT (RFC 6598)
		"100.127.255.255",    // CGNAT upper bound
		"198.18.0.1",         // Benchmark (RFC 2544)
		"192.0.2.1",          // TEST-NET-1 (RFC 5737)
		"198.51.100.1",       // TEST-NET-2 (RFC 5737)
		"203.0.113.1",        // TEST-NET-3 (RFC 5737)
		"240.0.0.1",          // Reserved (RFC 1112)
		"255.255.255.255",    // Limited broadcast
		"224.0.0.1",          // Multicast
		"239.255.255.250",    // Multicast
		"::1",                // IPv6 loopback
		"::",                 // IPv6 unspecified
		"::a9fe:a9fe",        // IPv4-compatible 169.254.169.254
		"::c0a8:100",         // IPv4-compatible 192.168.1.0
		"fe80::1",            // IPv6 link-local
		"ff02::1",            // IPv6 multicast
		"64:ff9b::192.0.2.1", // IPv4-IPv6 translation
		"2001::1",            // Teredo (RFC 4380)
		"2001:2::1",          // Benchmarking (RFC 5180)
		"2002:c000:0201::1",  // 6to4
		"3ffe::1",            // 6bone (RFC 3701)
	}

	for _, ipStr := range blocked {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			t.Fatalf("failed to parse IP %q", ipStr)
		}
		if !IsDisallowedIP(ip) {
			t.Errorf("IsDisallowedIP(%q) = false, want true", ipStr)
		}
	}

	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"2606:4700:4700::1111",
		"100.63.255.255", // Just below CGNAT
		"100.128.0.1",    // Just above CGNAT
		"198.20.0.1",     // Above benchmark
	}

	for _, ipStr := range allowed {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			t.Fatalf("failed to parse IP %q", ipStr)
		}
		if IsDisallowedIP(ip) {
			t.Errorf("IsDisallowedIP(%q) = true, want false", ipStr)
		}
	}

	if !IsDisallowedIP(nil) {
		t.Error("IsDisallowedIP(nil) = false, want true")
	}
}

func TestNormalizeTopic(t *testing.T) {
	if NormalizeTopic("") != "" {
		t.Errorf("NormalizeTopic(\"\") = %q, want \"\"", NormalizeTopic(""))
	}

	simple := "test-topic_123"
	if NormalizeTopic(simple) != simple {
		t.Errorf("NormalizeTopic(%q) = %q, want %q", simple, NormalizeTopic(simple), simple)
	}

	withSpecial := "provider:bluesky:alert!"
	expectedSpecial := "provider-bluesky-alert-"
	if NormalizeTopic(withSpecial) != expectedSpecial {
		t.Errorf("NormalizeTopic(%q) = %q, want %q", withSpecial, NormalizeTopic(withSpecial), expectedSpecial)
	}

	long := "this-is-a-very-long-topic-name-that-definitely-exceeds-thirty-two-bytes"
	norm := NormalizeTopic(long)
	if len(norm) > 32 {
		t.Errorf("NormalizeTopic(long) len = %d > 32 (%q)", len(norm), norm)
	}
	if !strings.HasPrefix(norm, "this-is-a-very-long-top-") {
		t.Errorf("NormalizeTopic(long) prefix mismatch: %q", norm)
	}

	// Distinct long topics with the same prefix must yield distinct normalized outputs
	long2 := "this-is-a-very-long-topic-name-that-definitely-exceeds-thirty-two-bytes-DIFFERENT"
	norm2 := NormalizeTopic(long2)
	if norm == norm2 {
		t.Errorf("expected distinct normalized topics for distinct inputs, got both %q", norm)
	}
}

func TestSend_DefaultClientBlocksRedirect(t *testing.T) {
	vapidKeys, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}

	redirectTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer server.Close()

	sub, _, _ := setupTestSubscription(t, server.URL)

	// Provide a client that allows loopback but uses the default CheckRedirect
	customClient := &http.Client{
		CheckRedirect: defaultCheckRedirect,
	}
	client := NewClient(ClientOptions{
		HTTPClient: customClient,
		VAPIDKeys:  *vapidKeys,
		Subject:    "mailto:admin@example.com",
	})

	err = client.Send(context.Background(), sub, []byte("test"), Options{TTL: 60})
	if err == nil {
		t.Fatal("expected redirect to be blocked, got nil")
	}
	if err.Error() != "webpush: push transport failed (details redacted)" {
		t.Errorf("expected redacted transport error, got: %v", err)
	}
}

func TestDecodeBase64Flex(t *testing.T) {
	raw := []byte("hello webpush test")
	encodings := []string{
		base64.RawURLEncoding.EncodeToString(raw),
		base64.URLEncoding.EncodeToString(raw),
		base64.RawStdEncoding.EncodeToString(raw),
		base64.StdEncoding.EncodeToString(raw),
	}

	for _, s := range encodings {
		decoded, err := DecodeBase64Flex(s)
		if err != nil {
			t.Errorf("DecodeBase64Flex(%q) error: %v", s, err)
		}
		if string(decoded) != string(raw) {
			t.Errorf("DecodeBase64Flex(%q) = %q, want %q", s, string(decoded), string(raw))
		}
	}

	if _, err := DecodeBase64Flex(""); err == nil {
		t.Error("expected error for empty string, got nil")
	}
	if _, err := DecodeBase64Flex("invalid!!base64"); err == nil {
		t.Error("expected error for invalid string, got nil")
	}
}
