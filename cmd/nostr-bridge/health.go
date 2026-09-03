package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	bridgeoauth "github.com/nakatanakatana/mytools/cmd/nostr-bridge/oauth"
)

const defaultJetstreamMaxEventAge = time.Minute

var providerRefreshReasons = [...]bridgeoauth.RefreshReason{
	bridgeoauth.RefreshReasonAuthorizationCode,
	bridgeoauth.RefreshReasonOnDemand,
	bridgeoauth.RefreshReasonMaintenance,
}

var providerRefreshErrorClasses = [...]bridgeoauth.RefreshErrorClass{
	bridgeoauth.RefreshErrorTimeout,
	bridgeoauth.RefreshErrorConnection,
	bridgeoauth.RefreshErrorRateLimit,
	bridgeoauth.RefreshErrorServer,
	bridgeoauth.RefreshErrorInvalidGrant,
	bridgeoauth.RefreshErrorMissingRefreshToken,
	bridgeoauth.RefreshErrorDecrypt,
	bridgeoauth.RefreshErrorDPoPKey,
	bridgeoauth.RefreshErrorProtocol,
}

// HealthMetrics is the non-sensitive operational state exported by Health.
type HealthMetrics struct {
	LastSync           time.Time
	LastJetstreamEvent time.Time
	JetstreamConnected bool
	TargetDIDCount     int
	PendingWork        int
	OAuthConnected     bool
	OAuthExpiry        time.Time
	OutboxCount        int64
	OutboxAtLimit      bool
	LastRelayDelivery  time.Time
	DispatcherRunning  bool
}

type ProviderHealthMetrics struct {
	AuthorizationAvailable, ReauthRequired bool
	Degraded, AccessTokenExpired           bool
	MaintenanceWorkerRunning               bool
	Bootstrapped, StreamConnected          bool
	OAuthExpiry                            time.Time
	TargetCount, PendingWork               int
	LastEvent, LastReconciliation          time.Time
	LastRefreshSucceededAt                 time.Time
	NextMaintenanceRefresh                 time.Time
	LastRefreshErrorClass                  bridgeoauth.RefreshErrorClass
	RefreshSuccesses                       map[bridgeoauth.RefreshReason]uint64
	RefreshFailures                        map[bridgeoauth.RefreshReason]map[bridgeoauth.RefreshErrorClass]uint64
	RefreshExecutions                      map[bridgeoauth.RefreshReason]uint64
}

// HealthOptions configures process health checks.
type HealthOptions struct {
	DatabaseCheck     func(context.Context) error
	Now               func() time.Time
	MaxEventAge       time.Duration
	OutboxCount       func(context.Context) (int64, error)
	OutboxLimit       int64
	RequireDispatcher bool
	EnabledProviders  []string
}

// Health serves liveness, readiness, and Prometheus-compatible metrics.
type Health struct {
	databaseCheck     func(context.Context) error
	now               func() time.Time
	maxEventAge       time.Duration
	outboxCount       func(context.Context) (int64, error)
	outboxLimit       int64
	requireDispatcher bool

	mu               sync.RWMutex
	metrics          HealthMetrics
	providers        map[string]ProviderHealthMetrics
	enabledProviders []string
}

type readinessSnapshot struct {
	Now             time.Time
	Metrics         HealthMetrics
	Providers       map[string]ProviderHealthMetrics
	ProviderReady   map[string]bool
	DatabaseReady   bool
	OAuthConnected  bool
	JetstreamReady  bool
	ProvidersReady  bool
	OutboxReady     bool
	OutboxCount     int64
	OutboxAtLimit   bool
	DispatcherReady bool
	Ready           bool
}

// NewHealth creates a health reporter. Metrics are initially zero-valued until
// the OAuth and Jetstream runtimes report their state.
func NewHealth(options HealthOptions) *Health {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.MaxEventAge <= 0 {
		options.MaxEventAge = defaultJetstreamMaxEventAge
	}
	return &Health{databaseCheck: options.DatabaseCheck, now: options.Now, maxEventAge: options.MaxEventAge, outboxCount: options.OutboxCount, outboxLimit: options.OutboxLimit, requireDispatcher: options.RequireDispatcher, providers: map[string]ProviderHealthMetrics{}, enabledProviders: boundedEnabledProviders(options.EnabledProviders)}
}

func boundedEnabledProviders(providers []string) []string {
	result := make([]string, 0, len(providers))
	seen := map[string]bool{}
	for _, provider := range providers {
		if (provider == "bluesky" || provider == "mastodon") && !seen[provider] {
			result = append(result, provider)
			seen[provider] = true
		}
	}
	return result
}

func (h *Health) UpdateProvider(provider string, update func(*ProviderHealthMetrics)) {
	if provider != "bluesky" && provider != "mastodon" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.providers[provider]
	update(&m)
	h.providers[provider] = m
}

func (h *Health) providerSnapshot(provider string) ProviderHealthMetrics {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.providers[provider]
}

// SetMetrics replaces the current public operational state.
func (h *Health) SetMetrics(metrics HealthMetrics) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.metrics = metrics
}

// Update applies a small runtime change without losing concurrent component state.
func (h *Health) Update(update func(*HealthMetrics)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	update(&h.metrics)
}

func (h *Health) snapshot() HealthMetrics {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.metrics
}

// RegisterHealthRoutes attaches the process endpoints to mux.
func RegisterHealthRoutes(mux *http.ServeMux, health *Health) {
	mux.HandleFunc("GET /healthz", health.Liveness)
	mux.HandleFunc("GET /readyz", health.Readiness)
	mux.HandleFunc("GET /metrics", health.Metrics)
}

// Liveness reports whether the HTTP process is running.
func (h *Health) Liveness(w http.ResponseWriter, _ *http.Request) {
	writeHealthJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readiness reports whether durable storage, OAuth, and Jetstream are ready.
func (h *Health) Readiness(w http.ResponseWriter, r *http.Request) {
	snapshot := h.readinessSnapshot(r.Context())
	providerStatus := map[string]any{}
	for _, provider := range h.enabledProviders {
		m := snapshot.Providers[provider]
		providerStatus[provider] = map[string]any{
			"authorization_available":    m.AuthorizationAvailable,
			"reauth_required":            m.ReauthRequired,
			"degraded":                   m.Degraded,
			"access_token_expired":       m.AccessTokenExpired,
			"maintenance_worker_running": m.MaintenanceWorkerRunning,
			"bootstrapped":               m.Bootstrapped,
			"stream_connected":           m.StreamConnected,
			"target_count":               m.TargetCount,
		}
	}
	status := http.StatusOK
	if !snapshot.Ready {
		status = http.StatusServiceUnavailable
	}
	writeHealthJSON(w, status, map[string]any{
		"ready":               snapshot.Ready,
		"database":            snapshot.DatabaseReady,
		"oauth_connected":     snapshot.OAuthConnected,
		"jetstream_connected": snapshot.Metrics.JetstreamConnected,
		"jetstream_required":  snapshot.Metrics.TargetDIDCount > 0,
		"last_event_age_ms":   jetstreamAgeMilliseconds(snapshot.Now, snapshot.Metrics.LastJetstreamEvent),
		"outbox_count":        snapshot.OutboxCount, "outbox_ready": snapshot.OutboxReady,
		"dispatcher_running": snapshot.DispatcherReady,
		"providers":          providerStatus,
	})
}

func (h *Health) readinessSnapshot(ctx context.Context) readinessSnapshot {
	metrics := h.snapshot()
	snapshot := readinessSnapshot{
		Metrics:         metrics,
		Providers:       make(map[string]ProviderHealthMetrics, len(h.enabledProviders)),
		ProviderReady:   make(map[string]bool, len(h.enabledProviders)),
		OutboxReady:     true,
		OutboxCount:     metrics.OutboxCount,
		OutboxAtLimit:   metrics.OutboxAtLimit,
		ProvidersReady:  true,
		DispatcherReady: !h.requireDispatcher || metrics.DispatcherRunning,
	}
	if h.outboxCount != nil {
		count, err := h.outboxCount(ctx)
		snapshot.OutboxCount = count
		snapshot.OutboxReady = err == nil && h.outboxLimit > 0 && count < h.outboxLimit
		snapshot.OutboxAtLimit = err == nil && h.outboxLimit > 0 && count >= h.outboxLimit
		snapshot.Metrics.OutboxCount = count
		snapshot.Metrics.OutboxAtLimit = snapshot.OutboxAtLimit
	}

	now := h.now()
	snapshot.Now = now
	snapshot.DatabaseReady = h.databaseCheck != nil && h.databaseCheck(ctx) == nil
	snapshot.OAuthConnected = metrics.OAuthConnected && (metrics.OAuthExpiry.IsZero() || metrics.OAuthExpiry.After(now))
	snapshot.JetstreamReady = metrics.TargetDIDCount == 0 || metrics.JetstreamConnected
	providerAuthorizationsReady := true
	if len(h.enabledProviders) > 0 {
		h.mu.RLock()
		for _, provider := range h.enabledProviders {
			m := h.providers[provider]
			snapshot.Providers[provider] = m
			authorizationReady := m.AuthorizationAvailable && !m.ReauthRequired
			if provider == "bluesky" {
				authorizationReady = authorizationReady && m.MaintenanceWorkerRunning
			} else {
				authorizationReady = authorizationReady && (m.OAuthExpiry.IsZero() || m.OAuthExpiry.After(now))
			}
			snapshot.ProviderReady[provider] = authorizationReady && m.Bootstrapped && (m.TargetCount == 0 || m.StreamConnected)
			providerAuthorizationsReady = providerAuthorizationsReady && authorizationReady
			snapshot.ProvidersReady = snapshot.ProvidersReady && snapshot.ProviderReady[provider]
		}
		h.mu.RUnlock()
		snapshot.OAuthConnected = providerAuthorizationsReady
		snapshot.JetstreamReady = true
	}
	snapshot.Ready = snapshot.DatabaseReady && snapshot.OAuthConnected && snapshot.JetstreamReady && snapshot.ProvidersReady && snapshot.OutboxReady && snapshot.DispatcherReady
	return snapshot
}

func jetstreamAgeMilliseconds(now, lastEvent time.Time) int64 {
	if lastEvent.IsZero() {
		return -1
	}
	return now.Sub(lastEvent).Milliseconds()
}

// Metrics exposes Prometheus text-format gauges and counters without tokens,
// keys, DIDs, or other secret material.
func (h *Health) Metrics(w http.ResponseWriter, r *http.Request) {
	metrics := h.snapshot()
	if h.outboxCount != nil {
		if count, err := h.outboxCount(r.Context()); err == nil {
			metrics.OutboxCount = count
			metrics.OutboxAtLimit = h.outboxLimit > 0 && count >= h.outboxLimit
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "nostr_bridge_last_sync_timestamp_seconds %.3f\n", unixSeconds(metrics.LastSync))
	_, _ = fmt.Fprintf(w, "nostr_bridge_jetstream_connected %d\n", boolMetric(metrics.JetstreamConnected))
	_, _ = fmt.Fprintf(w, "nostr_bridge_target_dids %d\n", metrics.TargetDIDCount)
	_, _ = fmt.Fprintf(w, "nostr_bridge_pending_work %d\n", metrics.PendingWork)
	_, _ = fmt.Fprintf(w, "nostr_bridge_oauth_expiry_timestamp_seconds %.3f\n", unixSeconds(metrics.OAuthExpiry))
	_, _ = fmt.Fprintf(w, "nostr_bridge_outbox_items %d\n", metrics.OutboxCount)
	_, _ = fmt.Fprintf(w, "nostr_bridge_outbox_at_limit %d\n", boolMetric(metrics.OutboxAtLimit))
	_, _ = fmt.Fprintf(w, "nostr_bridge_last_relay_delivery_timestamp_seconds %.3f\n", unixSeconds(metrics.LastRelayDelivery))
	_, _ = fmt.Fprintf(w, "nostr_bridge_dispatcher_running %d\n", boolMetric(metrics.DispatcherRunning))
	h.mu.RLock()
	for _, provider := range h.enabledProviders {
		m := h.providers[provider]
		label := fmt.Sprintf("{provider=%q}", provider)
		_, _ = fmt.Fprintf(w, "nostr_bridge_provider_authorization_available%s %d\n", label, boolMetric(m.AuthorizationAvailable))
		_, _ = fmt.Fprintf(w, "nostr_bridge_provider_bootstrapped%s %d\n", label, boolMetric(m.Bootstrapped))
		_, _ = fmt.Fprintf(w, "nostr_bridge_provider_stream_connected%s %d\n", label, boolMetric(m.StreamConnected))
		_, _ = fmt.Fprintf(w, "nostr_bridge_provider_targets%s %d\n", label, m.TargetCount)
		_, _ = fmt.Fprintf(w, "nostr_bridge_provider_pending_work%s %d\n", label, m.PendingWork)
		_, _ = fmt.Fprintf(w, "nostr_bridge_provider_last_event_timestamp_seconds%s %.3f\n", label, unixSeconds(m.LastEvent))
		_, _ = fmt.Fprintf(w, "nostr_bridge_provider_last_reconciliation_timestamp_seconds%s %.3f\n", label, unixSeconds(m.LastReconciliation))
		if provider == "bluesky" {
			_, _ = fmt.Fprintf(w, "nostr_bridge_provider_oauth_last_success_timestamp_seconds%s %.3f\n", label, unixSeconds(m.LastRefreshSucceededAt))
			_, _ = fmt.Fprintf(w, "nostr_bridge_provider_oauth_next_refresh_timestamp_seconds%s %.3f\n", label, unixSeconds(m.NextMaintenanceRefresh))
			_, _ = fmt.Fprintf(w, "nostr_bridge_provider_oauth_reauth_required%s %d\n", label, boolMetric(m.ReauthRequired))
			_, _ = fmt.Fprintf(w, "nostr_bridge_provider_oauth_degraded%s %d\n", label, boolMetric(m.Degraded))
			_, _ = fmt.Fprintf(w, "nostr_bridge_provider_oauth_access_token_expired%s %d\n", label, boolMetric(m.AccessTokenExpired))
			_, _ = fmt.Fprintf(w, "nostr_bridge_provider_oauth_maintenance_worker_running%s %d\n", label, boolMetric(m.MaintenanceWorkerRunning))
			for _, reason := range providerRefreshReasons {
				reasonLabel := fmt.Sprintf("{provider=%q,reason=%q}", provider, reason)
				_, _ = fmt.Fprintf(w, "nostr_bridge_provider_oauth_refresh_success_total%s %d\n", reasonLabel, m.RefreshSuccesses[reason])
				_, _ = fmt.Fprintf(w, "nostr_bridge_provider_oauth_refresh_executions_total%s %d\n", reasonLabel, m.RefreshExecutions[reason])
				for _, class := range providerRefreshErrorClasses {
					failureLabel := fmt.Sprintf("{provider=%q,reason=%q,class=%q}", provider, reason, class)
					_, _ = fmt.Fprintf(w, "nostr_bridge_provider_oauth_refresh_failure_total%s %d\n", failureLabel, providerRefreshFailureCount(m, reason, class))
				}
			}
			for _, class := range providerRefreshErrorClasses {
				classLabel := fmt.Sprintf("{provider=%q,class=%q}", provider, class)
				_, _ = fmt.Fprintf(
					w,
					"nostr_bridge_provider_oauth_last_error_class%s %d\n",
					classLabel,
					boolMetric(m.LastRefreshErrorClass == class),
				)
			}
		}
	}
	h.mu.RUnlock()
}

func providerRefreshFailureCount(
	metrics ProviderHealthMetrics,
	reason bridgeoauth.RefreshReason,
	class bridgeoauth.RefreshErrorClass,
) uint64 {
	byClass := metrics.RefreshFailures[reason]
	if byClass == nil {
		return 0
	}
	return byClass[class]
}

func isProviderRefreshReason(reason bridgeoauth.RefreshReason) bool {
	for _, candidate := range providerRefreshReasons {
		if candidate == reason {
			return true
		}
	}
	return false
}

func boundedProviderRefreshErrorClass(class bridgeoauth.RefreshErrorClass) bridgeoauth.RefreshErrorClass {
	for _, candidate := range providerRefreshErrorClasses {
		if candidate == class {
			return class
		}
	}
	return bridgeoauth.RefreshErrorProtocol
}

func unixSeconds(value time.Time) float64 {
	if value.IsZero() {
		return 0
	}
	return float64(value.UnixNano()) / float64(time.Second)
}

func boolMetric(value bool) int {
	if value {
		return 1
	}
	return 0
}

func writeHealthJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
