package main

import (
	"net/http"
	"time"
)

type statusResponse struct {
	Ready                bool                      `json:"ready"`
	Database             bool                      `json:"database"`
	OAuthConnected       bool                      `json:"oauth_connected"`
	JetstreamConnected   bool                      `json:"jetstream_connected"`
	JetstreamRequired    bool                      `json:"jetstream_required"`
	PendingWork          int                       `json:"pending_work"`
	LastJetstreamEventAt *time.Time                `json:"last_jetstream_event_at"`
	LastSyncAt           *time.Time                `json:"last_sync_at"`
	LastRelayDeliveryAt  *time.Time                `json:"last_relay_delivery_at"`
	DispatcherRunning    bool                      `json:"dispatcher_running"`
	Outbox               statusOutbox              `json:"outbox"`
	Providers            map[string]statusProvider `json:"providers"`
}

type statusOutbox struct {
	Count   int64 `json:"count"`
	Limit   int64 `json:"limit"`
	Ready   bool  `json:"ready"`
	AtLimit bool  `json:"at_limit"`
}

type statusProvider struct {
	Ready                    bool       `json:"ready"`
	AuthorizationAvailable   bool       `json:"authorization_available"`
	ReauthRequired           bool       `json:"reauth_required"`
	Degraded                 bool       `json:"degraded"`
	AccessTokenExpired       bool       `json:"access_token_expired"`
	MaintenanceWorkerRunning bool       `json:"maintenance_worker_running"`
	Bootstrapped             bool       `json:"bootstrapped"`
	StreamConnected          bool       `json:"stream_connected"`
	OAuthExpiryAt            *time.Time `json:"oauth_expiry_at"`
	TargetCount              int        `json:"target_count"`
	PendingWork              int        `json:"pending_work"`
	LastEventAt              *time.Time `json:"last_event_at"`
	LastReconciliationAt     *time.Time `json:"last_reconciliation_at"`
}

// RegisterStatusRoutes attaches the sanitized UI status endpoint to mux.
func RegisterStatusRoutes(mux *http.ServeMux, health *Health) {
	mux.HandleFunc("GET /api/status", health.Status)
}

// Status reports a sanitized operational snapshot for the embedded UI.
func (h *Health) Status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	snapshot := h.readinessSnapshot(r.Context())
	providers := make(map[string]statusProvider, len(h.enabledProviders))
	for _, name := range h.enabledProviders {
		metrics := snapshot.Providers[name]
		providers[name] = statusProvider{
			Ready:                    snapshot.ProviderReady[name],
			AuthorizationAvailable:   metrics.AuthorizationAvailable,
			ReauthRequired:           metrics.ReauthRequired,
			Degraded:                 metrics.Degraded,
			AccessTokenExpired:       metrics.AccessTokenExpired,
			MaintenanceWorkerRunning: metrics.MaintenanceWorkerRunning,
			Bootstrapped:             metrics.Bootstrapped,
			StreamConnected:          metrics.StreamConnected,
			OAuthExpiryAt:            nullableTime(metrics.OAuthExpiry),
			TargetCount:              metrics.TargetCount,
			PendingWork:              metrics.PendingWork,
			LastEventAt:              nullableTime(metrics.LastEvent),
			LastReconciliationAt:     nullableTime(metrics.LastReconciliation),
		}
	}
	writeHealthJSON(w, http.StatusOK, statusResponse{
		Ready:                snapshot.Ready,
		Database:             snapshot.DatabaseReady,
		OAuthConnected:       snapshot.OAuthConnected,
		JetstreamConnected:   snapshot.Metrics.JetstreamConnected,
		JetstreamRequired:    snapshot.Metrics.TargetDIDCount > 0,
		PendingWork:          snapshot.Metrics.PendingWork,
		LastJetstreamEventAt: nullableTime(snapshot.Metrics.LastJetstreamEvent),
		LastSyncAt:           nullableTime(snapshot.Metrics.LastSync),
		LastRelayDeliveryAt:  nullableTime(snapshot.Metrics.LastRelayDelivery),
		DispatcherRunning:    snapshot.Metrics.DispatcherRunning,
		Outbox: statusOutbox{
			Count:   snapshot.OutboxCount,
			Limit:   h.outboxLimit,
			Ready:   snapshot.OutboxReady,
			AtLimit: snapshot.OutboxAtLimit,
		},
		Providers: providers,
	})
}

func nullableTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
