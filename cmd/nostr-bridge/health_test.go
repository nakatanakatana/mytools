package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bridgeoauth "github.com/nakatanakatana/mytools/cmd/nostr-bridge/oauth"
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

func TestHealthReadinessUsesBlueskyOAuthAuthorizationAvailability(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		set  func(*ProviderHealthMetrics)
		want int
	}{
		{
			name: "expired access token remains ready when refreshable",
			set: func(m *ProviderHealthMetrics) {
				m.AuthorizationAvailable = true
				m.AccessTokenExpired = true
			},
			want: http.StatusOK,
		},
		{
			name: "transient refresh failure remains ready while degraded",
			set: func(m *ProviderHealthMetrics) {
				m.AuthorizationAvailable = true
				m.AccessTokenExpired = true
				m.Degraded = true
			},
			want: http.StatusOK,
		},
		{
			name: "missing refresh token is not ready",
			set: func(m *ProviderHealthMetrics) {
				m.ReauthRequired = true
			},
			want: http.StatusServiceUnavailable,
		},
		{
			name: "decrypt failure is not ready",
			set: func(m *ProviderHealthMetrics) {
				m.ReauthRequired = true
				m.Degraded = true
			},
			want: http.StatusServiceUnavailable,
		},
		{
			name: "invalid DPoP key is not ready",
			set: func(m *ProviderHealthMetrics) {
				m.ReauthRequired = true
			},
			want: http.StatusServiceUnavailable,
		},
		{
			name: "persisted reauthorization requirement is not ready",
			set: func(m *ProviderHealthMetrics) {
				m.ReauthRequired = true
			},
			want: http.StatusServiceUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := NewHealth(HealthOptions{
				DatabaseCheck:    func(context.Context) error { return nil },
				Now:              func() time.Time { return now },
				EnabledProviders: []string{"bluesky"},
			})
			health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
				m.Bootstrapped = true
				m.MaintenanceWorkerRunning = true
				tt.set(m)
			})

			recorder := httptest.NewRecorder()
			health.Readiness(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if recorder.Code != tt.want {
				t.Fatalf("readiness status = %d, want %d, body = %s", recorder.Code, tt.want, recorder.Body.String())
			}
			for _, field := range []string{
				`"authorization_available"`,
				`"reauth_required"`,
				`"degraded"`,
				`"access_token_expired"`,
				`"maintenance_worker_running"`,
			} {
				if !strings.Contains(recorder.Body.String(), field) {
					t.Errorf("readiness body missing %s: %s", field, recorder.Body.String())
				}
			}
		})
	}
}

func TestHealthReadinessRequiresBlueskyOAuthMaintenanceWorker(t *testing.T) {
	health := NewHealth(HealthOptions{
		DatabaseCheck:    func(context.Context) error { return nil },
		EnabledProviders: []string{"bluesky"},
	})
	health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		m.AuthorizationAvailable = true
		m.Bootstrapped = true
	})

	recorder := httptest.NewRecorder()
	health.Readiness(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("stopped maintenance worker readiness status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestHealthReadinessPreservesMastodonOAuthExpiry(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	health := NewHealth(HealthOptions{
		DatabaseCheck:    func(context.Context) error { return nil },
		Now:              func() time.Time { return now },
		EnabledProviders: []string{"mastodon"},
	})
	health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) {
		m.AuthorizationAvailable = true
		m.Bootstrapped = true
		m.OAuthExpiry = now.Add(-time.Nanosecond)
	})

	expired := httptest.NewRecorder()
	health.Readiness(expired, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if expired.Code != http.StatusServiceUnavailable {
		t.Fatalf("expired Mastodon readiness status = %d, body = %s", expired.Code, expired.Body.String())
	}

	health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) {
		m.OAuthExpiry = now.Add(time.Nanosecond)
	})
	valid := httptest.NewRecorder()
	health.Readiness(valid, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if valid.Code != http.StatusOK {
		t.Fatalf("valid Mastodon readiness status = %d, body = %s", valid.Code, valid.Body.String())
	}
}

type countingReadinessTokenProvider struct {
	token       bridgeoauth.Token
	tokenErr    error
	status      bridgeoauth.Status
	statusErr   error
	tokenCalls  int
	statusCalls int
}

func (p *countingReadinessTokenProvider) TokenByAccountDID(context.Context, string) (bridgeoauth.Token, error) {
	p.tokenCalls++
	return p.token, p.tokenErr
}

func (p *countingReadinessTokenProvider) AuthorizationStatus(context.Context, string, time.Duration) (bridgeoauth.Status, error) {
	p.statusCalls++
	return p.status, p.statusErr
}

func TestReadinessDoesNotRefreshOrInspectOAuth(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	client := &countingReadinessTokenProvider{
		token:  bridgeoauth.Token{AccessToken: "access-secret", Expiry: now.Add(-time.Minute)},
		status: bridgeoauth.Status{AuthorizationAvailable: true},
	}
	health := NewHealth(HealthOptions{
		DatabaseCheck:    func(context.Context) error { return nil },
		Now:              func() time.Time { return now },
		EnabledProviders: []string{"bluesky"},
	})
	provider := healthBlueskyTokenProvider{
		tokens:        client,
		health:        health,
		refreshPeriod: 30 * 24 * time.Hour,
	}
	_, _ = provider.TokenByAccountDID(context.Background(), "did:plc:fixture-secret")
	health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		m.Bootstrapped = true
		m.MaintenanceWorkerRunning = true
	})
	beforeToken, beforeStatus := client.tokenCalls, client.statusCalls
	if beforeToken != 1 || beforeStatus != 1 {
		t.Fatalf("cache priming calls = token %d, status %d; want 1 each", beforeToken, beforeStatus)
	}

	recorder := httptest.NewRecorder()
	health.Readiness(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("readiness status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if client.tokenCalls != beforeToken || client.statusCalls != beforeStatus {
		t.Fatalf(
			"readiness invoked OAuth: token calls %d -> %d, status calls %d -> %d",
			beforeToken, client.tokenCalls, beforeStatus, client.statusCalls,
		)
	}
	if strings.Contains(recorder.Body.String(), "did:plc:fixture-secret") || strings.Contains(recorder.Body.String(), "access-secret") {
		t.Fatalf("readiness leaked fixture material: %s", recorder.Body.String())
	}
}

func TestHealthMetricsExposeOperationalValuesWithoutSecretMaterial(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	health := NewHealth(HealthOptions{
		Now:              func() time.Time { return now },
		EnabledProviders: []string{"bluesky"},
	})
	health.SetMetrics(HealthMetrics{
		LastSync:           now.Add(-time.Minute),
		JetstreamConnected: true,
		TargetDIDCount:     2,
		PendingWork:        3,
		OAuthExpiry:        now.Add(time.Hour),
	})
	health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		m.AuthorizationAvailable = true
		m.ReauthRequired = false
		m.AccessTokenExpired = true
		m.MaintenanceWorkerRunning = true
		m.LastRefreshSucceededAt = now.Add(-time.Hour)
		m.NextMaintenanceRefresh = now.Add(29 * 24 * time.Hour)
		m.LastRefreshErrorClass = bridgeoauth.RefreshErrorServer
		m.RefreshSuccesses = map[bridgeoauth.RefreshReason]uint64{
			bridgeoauth.RefreshReasonMaintenance:                2,
			bridgeoauth.RefreshReason("did:plc:fixture-secret"): 999,
		}
		m.RefreshFailures = map[bridgeoauth.RefreshReason]map[bridgeoauth.RefreshErrorClass]uint64{
			bridgeoauth.RefreshReasonMaintenance: {
				bridgeoauth.RefreshErrorServer: 1,
			},
			bridgeoauth.RefreshReason("access-secret"): {
				bridgeoauth.RefreshErrorClass("refresh-secret"): 999,
			},
		}
		m.RefreshExecutions = map[bridgeoauth.RefreshReason]uint64{
			bridgeoauth.RefreshReasonMaintenance:                  3,
			bridgeoauth.RefreshReason("encrypted-payload-secret"): 999,
		}
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
		"nostr_bridge_provider_authorization_available",
		"nostr_bridge_provider_oauth_refresh_success_total",
		"nostr_bridge_provider_oauth_refresh_failure_total",
		"nostr_bridge_provider_oauth_last_success_timestamp_seconds",
		"nostr_bridge_provider_oauth_next_refresh_timestamp_seconds",
		"nostr_bridge_provider_oauth_last_error_class",
		"nostr_bridge_provider_oauth_reauth_required",
		"nostr_bridge_provider_oauth_access_token_expired",
		"nostr_bridge_provider_oauth_maintenance_worker_running",
		"nostr_bridge_provider_oauth_refresh_executions_total",
	} {
		if !strings.Contains(recorder.Body.String(), name) {
			t.Errorf("metrics missing %q: %s", name, recorder.Body.String())
		}
	}
	for _, sample := range []string{
		`nostr_bridge_provider_oauth_refresh_success_total{provider="bluesky",reason="maintenance"} 2`,
		`nostr_bridge_provider_oauth_refresh_failure_total{provider="bluesky",reason="maintenance",class="server"} 1`,
		`nostr_bridge_provider_oauth_refresh_success_total{provider="bluesky",reason="authorization_code"} 0`,
		`nostr_bridge_provider_oauth_refresh_failure_total{provider="bluesky",reason="on_demand",class="timeout"} 0`,
		`nostr_bridge_provider_oauth_last_error_class{provider="bluesky",class="server"} 1`,
		`nostr_bridge_provider_oauth_last_error_class{provider="bluesky",class="timeout"} 0`,
		`nostr_bridge_provider_oauth_refresh_executions_total{provider="bluesky",reason="maintenance"} 3`,
	} {
		if !strings.Contains(recorder.Body.String(), sample) {
			t.Errorf("metrics missing sample %q: %s", sample, recorder.Body.String())
		}
	}
	if strings.Contains(recorder.Body.String(), "nostr_bridge_provider_authenticated") {
		t.Fatalf("metrics retained ambiguous authenticated gauge: %s", recorder.Body.String())
	}
	for _, secret := range []string{
		"did:plc:fixture-secret",
		"access-secret",
		"refresh-secret",
		"encrypted-payload-secret",
	} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("metrics leaked %q: %s", secret, recorder.Body.String())
		}
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

func TestReadinessRequiresEveryTargetedEnabledProviderStreamConnection(t *testing.T) {
	health := NewHealth(HealthOptions{DatabaseCheck: func(context.Context) error { return nil }, EnabledProviders: []string{"bluesky", "mastodon"}})
	health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		m.AuthorizationAvailable = true
		m.MaintenanceWorkerRunning = true
		m.Bootstrapped = true
	})
	health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) { m.AuthorizationAvailable = true })
	r := httptest.NewRecorder()
	health.Readiness(r, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", r.Code, r.Body.String())
	}
	health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) { m.Bootstrapped = true; m.TargetCount = 2; m.StreamConnected = false })
	r = httptest.NewRecorder()
	health.Readiness(r, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("targeted disconnected stream status = %d, body = %s", r.Code, r.Body.String())
	}
	health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) { m.StreamConnected = true })
	r = httptest.NewRecorder()
	health.Readiness(r, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if r.Code != http.StatusOK {
		t.Fatalf("targeted connected stream status = %d, body = %s", r.Code, r.Body.String())
	}
	health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) { m.TargetCount = 0; m.StreamConnected = false })
	r = httptest.NewRecorder()
	health.Readiness(r, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if r.Code != http.StatusOK {
		t.Fatalf("zero-target disconnected stream status = %d, body = %s", r.Code, r.Body.String())
	}
}

func TestProviderMetricsUseOnlyBoundedProviderLabels(t *testing.T) {
	health := NewHealth(HealthOptions{EnabledProviders: []string{"mastodon", "did:plc:fixture-secret", "bluesky"}})
	health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) { m.AuthorizationAvailable = true; m.TargetCount = 2 })
	r := httptest.NewRecorder()
	health.Metrics(r, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := r.Body.String()
	for _, label := range []string{`provider="bluesky"`, `provider="mastodon"`} {
		if !strings.Contains(body, label) {
			t.Fatalf("missing %s: %s", label, body)
		}
	}
	if strings.Contains(body, "social.example") || strings.Contains(body, "did:plc") {
		t.Fatalf("unbounded label leaked: %s", body)
	}
}
