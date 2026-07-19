package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthLivenessIsAlwaysAvailable(t *testing.T) {
	health := NewHealth(HealthOptions{})
	recorder := httptest.NewRecorder()

	health.Liveness(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("liveness status = %d, want %d", recorder.Code, http.StatusOK)
	}
}

func TestHealthReadinessRequiresDatabaseOAuthAndConnectedJetstreamForTargets(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	health := NewHealth(HealthOptions{
		DatabaseCheck: func(context.Context) error { return nil },
		Now:           func() time.Time { return now },
		MaxEventAge:   time.Minute,
	})
	health.SetMetrics(HealthMetrics{OAuthConnected: true, JetstreamConnected: true, LastJetstreamEvent: now.Add(-24 * time.Hour), TargetDIDCount: 1})

	ready := httptest.NewRecorder()
	health.Readiness(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("connected readiness status = %d, body = %s", ready.Code, ready.Body.String())
	}

	health.SetMetrics(HealthMetrics{OAuthConnected: true, TargetDIDCount: 1})
	disconnected := httptest.NewRecorder()
	health.Readiness(disconnected, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if disconnected.Code != http.StatusServiceUnavailable {
		t.Fatalf("disconnected readiness status = %d, body = %s", disconnected.Code, disconnected.Body.String())
	}
}

func TestHealthReadinessDoesNotRequireJetstreamWithNoTargets(t *testing.T) {
	health := NewHealth(HealthOptions{DatabaseCheck: func(context.Context) error { return nil }})
	health.SetMetrics(HealthMetrics{OAuthConnected: true, TargetDIDCount: 0})
	recorder := httptest.NewRecorder()
	health.Readiness(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("zero-target readiness status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestHealthReadinessRejectsExpiredOAuthToken(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	health := NewHealth(HealthOptions{
		DatabaseCheck: func(context.Context) error { return nil },
		Now:           func() time.Time { return now },
		MaxEventAge:   time.Minute,
	})
	health.SetMetrics(HealthMetrics{
		OAuthConnected:     true,
		OAuthExpiry:        now.Add(-time.Nanosecond),
		JetstreamConnected: true,
		LastJetstreamEvent: now.Add(-time.Second),
	})

	recorder := httptest.NewRecorder()
	health.Readiness(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expired OAuth readiness status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"oauth_connected":false`) {
		t.Fatalf("expired OAuth reported as connected: %s", recorder.Body.String())
	}
}

func TestHealthMetricsExposeOperationalValuesWithoutSecretMaterial(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	health := NewHealth(HealthOptions{Now: func() time.Time { return now }})
	health.SetMetrics(HealthMetrics{
		LastSync:           now.Add(-time.Minute),
		JetstreamConnected: true,
		TargetDIDCount:     2,
		PendingWork:        3,
		OAuthExpiry:        now.Add(time.Hour),
	})
	recorder := httptest.NewRecorder()

	health.Metrics(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", recorder.Code)
	}
	for _, name := range []string{
		"nostr_bridge_last_sync_timestamp_seconds",
		"nostr_bridge_jetstream_connected",
		"nostr_bridge_target_dids",
		"nostr_bridge_pending_work",
		"nostr_bridge_oauth_expiry_timestamp_seconds",
		"nostr_bridge_outbox_at_limit",
	} {
		if !strings.Contains(recorder.Body.String(), name) {
			t.Errorf("metrics missing %q: %s", name, recorder.Body.String())
		}
	}
	if strings.Contains(recorder.Body.String(), "secret") {
		t.Fatalf("metrics leaked secret material: %s", recorder.Body.String())
	}
}

func TestRegisterHealthRoutes(t *testing.T) {
	mux := http.NewServeMux()
	RegisterHealthRoutes(mux, NewHealth(HealthOptions{}))
	recorder := httptest.NewRecorder()

	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("health route status = %d", recorder.Code)
	}
}

func TestReadinessFailsImmediatelyWhenDispatcherStops(t *testing.T) {
	now := time.Unix(100, 0)
	health := NewHealth(HealthOptions{DatabaseCheck: func(context.Context) error { return nil }, Now: func() time.Time { return now }, OutboxCount: func(context.Context) (int64, error) { return 0, nil }, OutboxLimit: 10, RequireDispatcher: true})
	health.SetMetrics(HealthMetrics{OAuthConnected: true, JetstreamConnected: true, LastJetstreamEvent: now, DispatcherRunning: false})
	recorder := httptest.NewRecorder()
	health.Readiness(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
}
