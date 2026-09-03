package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bridgeoauth "github.com/nakatanakatana/mytools/cmd/nostr-bridge/oauth"
)

func TestStatusReturnsProviderStateAndNullableTimes(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	health := NewHealth(HealthOptions{
		Now:               func() time.Time { return now },
		DatabaseCheck:     func(context.Context) error { return nil },
		OutboxCount:       func(context.Context) (int64, error) { return 3, nil },
		OutboxLimit:       100,
		RequireDispatcher: true,
		EnabledProviders:  []string{"bluesky", "mastodon"},
	})
	health.SetMetrics(HealthMetrics{
		LastSync:           now.Add(-time.Minute),
		LastJetstreamEvent: now.Add(-10 * time.Second),
		JetstreamConnected: true,
		OAuthConnected:     true,
		PendingWork:        2,
		LastRelayDelivery:  now.Add(-5 * time.Second),
		DispatcherRunning:  true,
	})
	health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		m.AuthorizationAvailable = true
		m.MaintenanceWorkerRunning = true
		m.Bootstrapped = true
		m.StreamConnected = true
		m.TargetCount = 4
		m.LastEvent = now.Add(-8 * time.Second)
		m.LastReconciliation = now.Add(-2 * time.Minute)
	})
	health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) {
		m.AuthorizationAvailable = true
		m.Bootstrapped = true
	})

	recorder := httptest.NewRecorder()
	health.Status(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var got statusResponse
	if err := json.NewDecoder(recorder.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Ready || !got.Database || !got.OAuthConnected || !got.JetstreamConnected || !got.DispatcherRunning {
		t.Fatalf("top-level status = %#v", got)
	}
	if got.PendingWork != 2 || got.Outbox.Count != 3 || got.Outbox.Limit != 100 || !got.Outbox.Ready || got.Outbox.AtLimit {
		t.Fatalf("work status = %#v", got)
	}
	provider := got.Providers["bluesky"]
	if !provider.Ready || !provider.AuthorizationAvailable || !provider.Bootstrapped || !provider.StreamConnected || provider.TargetCount != 4 || provider.PendingWork != 0 {
		t.Fatalf("Bluesky status = %#v", provider)
	}
	if provider.LastEventAt == nil || provider.LastReconciliationAt == nil {
		t.Fatalf("Bluesky timestamps = %#v", provider)
	}
	if got.Providers["mastodon"].LastEventAt != nil || got.Providers["mastodon"].OAuthExpiryAt != nil {
		t.Fatalf("uninitialized Mastodon timestamps = %#v", got.Providers["mastodon"])
	}
}

func TestStatusReportsDegradedProviderWithHTTP200(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		update func(*ProviderHealthMetrics)
	}{
		{name: "reauth required", update: func(m *ProviderHealthMetrics) { m.ReauthRequired = true }},
		{name: "stream disconnected", update: func(m *ProviderHealthMetrics) { m.StreamConnected = false }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			health := readyStatusHealth(now)
			health.UpdateProvider("bluesky", tt.update)
			recorder := httptest.NewRecorder()
			health.Status(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status code = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			var got statusResponse
			if err := json.NewDecoder(recorder.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Ready || got.Providers["bluesky"].Ready {
				t.Fatalf("degraded status = %#v", got)
			}
		})
	}
}

func TestStatusOutboxCountErrorIsNotAtLimit(t *testing.T) {
	health := NewHealth(HealthOptions{
		DatabaseCheck: func(context.Context) error { return nil },
		OutboxCount: func(context.Context) (int64, error) {
			return 100, errors.New("outbox-count-secret")
		},
		OutboxLimit: 100,
	})
	health.SetMetrics(HealthMetrics{OAuthConnected: true})

	recorder := httptest.NewRecorder()
	health.Status(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "outbox-count-secret") {
		t.Fatalf("status leaked outbox error: %s", recorder.Body.String())
	}
	var got statusResponse
	if err := json.NewDecoder(recorder.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Ready || got.Outbox.Ready || got.Outbox.AtLimit {
		t.Fatalf("outbox error status = %#v", got)
	}
}

func TestStatusSanitizesFailuresAndZeroTimes(t *testing.T) {
	for _, tt := range []struct {
		name          string
		databaseCheck func(context.Context) error
		outboxCount   func(context.Context) (int64, error)
	}{
		{name: "database failure", databaseCheck: func(context.Context) error { return errors.New("database-secret") }, outboxCount: func(context.Context) (int64, error) { return 0, nil }},
		{name: "outbox failure", databaseCheck: func(context.Context) error { return nil }, outboxCount: func(context.Context) (int64, error) { return 0, errors.New("outbox-secret") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			health := NewHealth(HealthOptions{
				DatabaseCheck: tt.databaseCheck,
				OutboxCount:   tt.outboxCount,
				OutboxLimit:   100,
			})
			health.SetMetrics(HealthMetrics{LastSync: time.Time{}, LastJetstreamEvent: time.Time{}, LastRelayDelivery: time.Time{}})
			recorder := httptest.NewRecorder()
			health.Status(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status code = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "database-secret") || strings.Contains(recorder.Body.String(), "outbox-secret") || strings.Contains(recorder.Body.String(), "0001-01-01") {
				t.Fatalf("status leaked failure or zero time: %s", recorder.Body.String())
			}
			var raw map[string]any
			if err := json.NewDecoder(strings.NewReader(recorder.Body.String())).Decode(&raw); err != nil {
				t.Fatal(err)
			}
			if raw["ready"] != false || raw["last_sync_at"] != nil || raw["last_jetstream_event_at"] != nil || raw["last_relay_delivery_at"] != nil {
				t.Fatalf("failure status = %#v", raw)
			}
		})
	}
}

func TestStatusDoesNotExposeSensitiveFixtureMaterial(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	health := readyStatusHealth(now)
	health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		m.RefreshSuccesses = map[bridgeoauth.RefreshReason]uint64{bridgeoauth.RefreshReason("refresh-counter-secret"): 42}
		m.RefreshFailures = map[bridgeoauth.RefreshReason]map[bridgeoauth.RefreshErrorClass]uint64{bridgeoauth.RefreshReason("oauth-state-secret"): {bridgeoauth.RefreshErrorClass("raw-error-secret"): 1}}
		m.RefreshExecutions = map[bridgeoauth.RefreshReason]uint64{bridgeoauth.RefreshReason("outbox-payload-secret"): 1}
	})
	recorder := httptest.NewRecorder()
	health.Status(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	for _, secret := range []string{"access-token-secret", "private-key-secret", "oauth-state-secret", "cursor-secret", "did:plc:target-secret", "outbox-payload-secret", "raw-error-secret", "refresh-counter-secret"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("status leaked %q: %s", secret, recorder.Body.String())
		}
	}
}

func TestRegisterStatusRoutes(t *testing.T) {
	mux := http.NewServeMux()
	RegisterStatusRoutes(mux, NewHealth(HealthOptions{}))
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status route status = %d", recorder.Code)
	}
}

func readyStatusHealth(now time.Time) *Health {
	health := NewHealth(HealthOptions{
		Now:               func() time.Time { return now },
		DatabaseCheck:     func(context.Context) error { return nil },
		OutboxCount:       func(context.Context) (int64, error) { return 0, nil },
		OutboxLimit:       100,
		RequireDispatcher: true,
		EnabledProviders:  []string{"bluesky"},
	})
	health.SetMetrics(HealthMetrics{DispatcherRunning: true})
	health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		m.AuthorizationAvailable = true
		m.MaintenanceWorkerRunning = true
		m.Bootstrapped = true
		m.StreamConnected = true
		m.TargetCount = 1
	})
	return health
}
