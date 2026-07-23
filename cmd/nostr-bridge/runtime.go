package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/bluesky"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/mastodon"
	bridgeoauth "github.com/nakatanakatana/mytools/cmd/nostr-bridge/oauth"
	bridgeowner "github.com/nakatanakatana/mytools/cmd/nostr-bridge/owner"
	neutral "github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/syncer"
)

// runtimeSync owns the authenticated source and Jetstream lifecycle.
type runtimeSync struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

type liveTargets struct {
	mu      sync.RWMutex
	values  bluesky.DIDSet
	updates chan struct{}
}

func (t *liveTargets) Set(values bluesky.DIDSet) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if didSetsEqual(t.values, values) {
		return
	}
	t.values = make(bluesky.DIDSet, len(values))
	for did := range values {
		t.values[did] = struct{}{}
	}
	select {
	case t.updates <- struct{}{}:
	default:
	}
}

func (t *liveTargets) Updates() <-chan struct{} { return t.updates }

func newLiveTargets(values bluesky.DIDSet) *liveTargets {
	targets := &liveTargets{updates: make(chan struct{}, 1)}
	targets.Set(values)
	select {
	case <-targets.updates:
	default:
	}
	return targets
}

func didSetsEqual(a, b bluesky.DIDSet) bool {
	if len(a) != len(b) {
		return false
	}
	for did := range a {
		if _, ok := b[did]; !ok {
			return false
		}
	}
	return true
}
func (t *liveTargets) Get() bluesky.DIDSet {
	t.mu.RLock()
	defer t.mu.RUnlock()
	values := make(bluesky.DIDSet, len(t.values))
	for did := range t.values {
		values[did] = struct{}{}
	}
	return values
}

var errInvalidMasterSeed = errors.New("bridge master seed must be base64 encoding of exactly 32 bytes")
var runtimeRetryDelay = 10 * time.Second

func waitRuntimeRetry(ctx context.Context) bool {
	timer := time.NewTimer(runtimeRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func withProviderPending(health *Health, provider string, work func() error) error {
	health.UpdateProvider(provider, func(m *ProviderHealthMetrics) { m.PendingWork++ })
	defer health.UpdateProvider(provider, func(m *ProviderHealthMetrics) {
		if m.PendingWork > 0 {
			m.PendingWork--
		}
	})
	return work()
}

type runtimeStore interface {
	bridgestore.SyncDeliveryStore
	bridgestore.ReconciliationStore
}

func newRuntimeSync(cfg Config, store runtimeStore, oauthClient *bridgeoauth.Client, mastodonOAuth *mastodon.OAuthClient, health *Health) (*runtimeSync, error) {
	seed, err := base64.StdEncoding.DecodeString(cfg.Shared.MasterSeed)
	if err != nil || len(seed) != 32 {
		return nil, errInvalidMasterSeed
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	runtime := &runtimeSync{cancel: cancel, done: done}
	go func() {
		defer close(done)
		runtime.run(ctx, cfg, seed, store, oauthClient, mastodonOAuth, health)
	}()
	return runtime, nil
}

func (r *runtimeSync) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return r.CloseContext(ctx)
}

func (r *runtimeSync) Cancel() { r.cancel() }

func (r *runtimeSync) Wait(ctx context.Context) error {
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *runtimeSync) CloseContext(ctx context.Context) error {
	r.Cancel()
	return r.Wait(ctx)
}

func (r *runtimeSync) run(ctx context.Context, cfg Config, seed []byte, store runtimeStore, oauthClient *bridgeoauth.Client, mastodonOAuth *mastodon.OAuthClient, health *Health) {
	coordinator := bridgeowner.New(bridgeowner.Options{MasterSeed: seed, OwnerID: cfg.Owner.ID, OwnerName: cfg.Owner.Name, OwnerAbout: cfg.Owner.About, OwnerPicture: cfg.Owner.Picture, Store: store, OutboxLimit: int64(cfg.Shared.OutboxLimit), EnabledScopes: enabledProviderScopes(cfg)})
	var wg sync.WaitGroup
	if cfg.Bluesky.Enabled() && oauthClient != nil {
		wg.Add(1)
		go func() { defer wg.Done(); r.runBluesky(ctx, cfg, seed, store, oauthClient, health, coordinator) }()
	}
	if cfg.Mastodon.Enabled() && mastodonOAuth != nil {
		wg.Add(1)
		go func() { defer wg.Done(); r.runMastodon(ctx, cfg, seed, store, mastodonOAuth, health, coordinator) }()
	}
	wg.Wait()
}

func enabledProviderScopes(cfg Config) []bridgestore.SourceScope {
	var scopes []bridgestore.SourceScope
	if cfg.Bluesky.Enabled() {
		scopes = append(scopes, bridgestore.SourceScope{Provider: "bluesky", Account: cfg.Bluesky.AccountDID})
	}
	if cfg.Mastodon.Enabled() {
		scopes = append(scopes, bridgestore.SourceScope{Provider: "mastodon", Account: normalizedMastodonAccount(cfg.Mastodon.Account, cfg.Mastodon.BaseURL)})
	}
	return scopes
}

type blueskyAuthorizationProvider interface {
	bluesky.TokenProvider
	AuthorizationStatus(context.Context, string, time.Duration) (bridgeoauth.Status, error)
}

type healthBlueskyTokenProvider struct {
	tokens        blueskyAuthorizationProvider
	health        *Health
	refreshPeriod time.Duration
}

func (p healthBlueskyTokenProvider) TokenByAccountDID(ctx context.Context, accountDID string) (bridgeoauth.Token, error) {
	token, err := p.tokens.TokenByAccountDID(ctx, accountDID)
	if status, statusErr := p.tokens.AuthorizationStatus(ctx, accountDID, p.refreshPeriod); statusErr == nil {
		p.health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
			applyBlueskyAuthorizationStatus(m, status)
		})
	}
	if err != nil {
		return bridgeoauth.Token{}, err
	}
	p.health.Update(func(metrics *HealthMetrics) {
		metrics.OAuthConnected = true
		metrics.OAuthExpiry = token.Expiry
	})
	p.health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		m.OAuthExpiry = token.Expiry
	})
	return token, nil
}

func applyBlueskyAuthorizationStatus(m *ProviderHealthMetrics, status bridgeoauth.Status) {
	m.AuthorizationAvailable = status.AuthorizationAvailable
	m.ReauthRequired = status.ReauthRequired
	m.Degraded = status.LastRefreshErrorClass != ""
	m.AccessTokenExpired = !status.AccessTokenValid
	m.LastRefreshSucceededAt = status.LastRefreshSucceededAt
	m.NextMaintenanceRefresh = status.NextMaintenanceRefresh
	if status.LastRefreshErrorClass == "" {
		m.LastRefreshErrorClass = ""
	} else {
		m.LastRefreshErrorClass = boundedProviderRefreshErrorClass(status.LastRefreshErrorClass)
	}
}

type healthOAuthMaintenanceObserver struct {
	health *Health
}

func (o healthOAuthMaintenanceObserver) Started(time.Time) {
	o.health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		m.MaintenanceWorkerRunning = true
	})
}

func (o healthOAuthMaintenanceObserver) Stopped(time.Time) {
	o.health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		m.MaintenanceWorkerRunning = false
	})
}

func (o healthOAuthMaintenanceObserver) Checked(_ time.Time, status bridgeoauth.Status) {
	o.health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		applyBlueskyAuthorizationStatus(m, status)
	})
}

func (o healthOAuthMaintenanceObserver) RefreshSucceeded(_ time.Time, reason bridgeoauth.RefreshReason) {
	if !isProviderRefreshReason(reason) {
		return
	}
	o.health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		ensureProviderRefreshCounters(m)
		m.RefreshSuccesses[reason]++
		m.RefreshExecutions[reason]++
		m.LastRefreshErrorClass = ""
		m.Degraded = false
	})
}

func (o healthOAuthMaintenanceObserver) RefreshFailed(
	_ time.Time,
	reason bridgeoauth.RefreshReason,
	class bridgeoauth.RefreshErrorClass,
) {
	if !isProviderRefreshReason(reason) {
		return
	}
	class = boundedProviderRefreshErrorClass(class)
	o.health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		ensureProviderRefreshCounters(m)
		if m.RefreshFailures[reason] == nil {
			m.RefreshFailures[reason] = map[bridgeoauth.RefreshErrorClass]uint64{}
		}
		m.RefreshFailures[reason][class]++
		m.RefreshExecutions[reason]++
		m.LastRefreshErrorClass = class
		m.Degraded = true
	})
}

func (healthOAuthMaintenanceObserver) RetryScheduled(
	time.Time,
	bridgeoauth.RefreshReason,
	bridgeoauth.RefreshErrorClass,
	time.Duration,
) {
}

func ensureProviderRefreshCounters(m *ProviderHealthMetrics) {
	if m.RefreshSuccesses == nil {
		m.RefreshSuccesses = map[bridgeoauth.RefreshReason]uint64{}
	}
	if m.RefreshFailures == nil {
		m.RefreshFailures = map[bridgeoauth.RefreshReason]map[bridgeoauth.RefreshErrorClass]uint64{}
	}
	if m.RefreshExecutions == nil {
		m.RefreshExecutions = map[bridgeoauth.RefreshReason]uint64{}
	}
}

func (r *runtimeSync) runBluesky(ctx context.Context, cfg Config, seed []byte, store runtimeStore, oauthClient *bridgeoauth.Client, health *Health, coordinator reconciliationCoordinator) {
	tokenProvider := healthBlueskyTokenProvider{tokens: oauthClient, health: health, refreshPeriod: cfg.Bluesky.OAuthRefreshPeriod}
	for ctx.Err() == nil {
		scope := bridgestore.SourceScope{Provider: "bluesky", Account: cfg.Bluesky.AccountDID}
		token, err := tokenProvider.TokenByAccountDID(ctx, cfg.Bluesky.AccountDID)
		if err == nil {
			if !token.Expiry.IsZero() && !token.Expiry.After(time.Now()) {
				health.Update(func(metrics *HealthMetrics) {
					metrics.OAuthConnected = false
					metrics.OAuthExpiry = token.Expiry
				})
				health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
					m.OAuthExpiry = token.Expiry
					m.AccessTokenExpired = true
					m.StreamConnected = false
				})
				if !waitRuntimeRetry(ctx) {
					return
				}
				continue
			}
			if source, sourceErr := bluesky.NewClient(bluesky.ClientOptions{BaseURL: cfg.Bluesky.BaseURL, Tokens: tokenProvider, AccountDID: cfg.Bluesky.AccountDID}); sourceErr == nil {
				health.Update(func(metrics *HealthMetrics) { metrics.PendingWork++ })
				var targets bluesky.TargetSet
				var reconcileErr error
				initialErr := withProviderPending(health, "bluesky", func() error {
					targets, reconcileErr = bluesky.NewReconciler(source, cfg.Bluesky.ListURIs).Reconcile(ctx)
					if reconcileErr != nil {
						reportReconciliationFailure("initial reconciliation", reconcileErr)
					}
					targets = resolveInitialTargets(ctx, store, scope, targets, reconcileErr, health)
					if reconcileErr == nil {
						err := reconcileBluesky(ctx, coordinator, source, scope, targets)
						if err != nil {
							reportReconciliationFailure("initial publication", err)
						}
						return err
					}
					return nil
				})
				health.Update(func(metrics *HealthMetrics) {
					if metrics.PendingWork > 0 {
						metrics.PendingWork--
					}
				})
				if initialErr != nil {
					if !waitRuntimeRetry(ctx) {
						return
					}
					continue
				}
				if reconcileErr == nil {
					health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
						m.Bootstrapped = true
						m.TargetCount = len(targets.Union)
						m.LastReconciliation = time.Now()
					})
				}
				if len(targets.Union) > 0 {
					live := newLiveTargets(targets.Union)
					syncContext, stopSync := context.WithCancel(ctx)
					reconcileDone := make(chan struct{})
					go func() {
						defer close(reconcileDone)
						reconcilePeriodically(syncContext, cfg.Bluesky.ReconcileInterval, source, coordinator, scope, cfg.Bluesky.ListURIs, store, cfg.Shared.OutboxLimit, live, health)
					}()
					s := syncer.New(syncer.Options{Scope: scope, Source: source, OutboxStore: store, OutboxLimit: int64(cfg.Shared.OutboxLimit), MasterSeed: seed, TargetProvider: live.Get, TargetUpdates: live.Updates(), BackfillLimit: cfg.Bluesky.BackfillLimit, JetstreamURL: cfg.Bluesky.JetstreamURL, Observer: healthSyncObserver{health}})
					reportSyncFailure(s.Run(syncContext))
					stopSync()
					<-reconcileDone
				}
			} else {
				reportRuntimeFailure("source construction", sourceErr)
			}
		} else {
			health.Update(func(metrics *HealthMetrics) { metrics.OAuthConnected = false; metrics.JetstreamConnected = false })
			health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) { m.StreamConnected = false })
		}
		if !waitRuntimeRetry(ctx) {
			return
		}
	}
}

func (r *runtimeSync) runMastodon(ctx context.Context, cfg Config, seed []byte, store runtimeStore, oauthClient *mastodon.OAuthClient, health *Health, coordinator reconciliationCoordinator) {
	scope := bridgestore.SourceScope{Provider: "mastodon", Account: normalizedMastodonAccount(cfg.Mastodon.Account, cfg.Mastodon.BaseURL)}
	for ctx.Err() == nil {
		token, err := oauthClient.Token(ctx)
		if err == nil {
			health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) { m.AuthorizationAvailable = true; m.OAuthExpiry = token.Expiry })
			var client *mastodon.Client
			var snapshot neutral.TargetSnapshot
			var profiles []neutral.Profile
			attemptErr := withProviderPending(health, "mastodon", func() error {
				var err error
				client, err = mastodon.NewClient(mastodon.ClientOptions{BaseURL: cfg.Mastodon.BaseURL, Tokens: oauthClient})
				if err != nil {
					reportMastodonFailure("client construction", err)
					return err
				}
				snapshot, profiles, err = mastodon.NewReconciler(client, cfg.Mastodon.ListIDs).Reconcile(ctx)
				if err != nil {
					reportMastodonFailure("initial reconciliation", err)
					return err
				}
				err = coordinator.Reconcile(ctx, scope, snapshot, profiles)
				if err != nil {
					reportMastodonFailure("initial publication", err)
				}
				return err
			})
			if attemptErr == nil {
				health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) {
					m.Bootstrapped = true
					m.TargetCount = len(snapshot.Union)
					m.LastReconciliation = time.Now()
				})
				targets := newNeutralLiveTargets(snapshot.Union)
				syncCtx, cancelSync := context.WithCancel(ctx)
				reconcileDone := make(chan struct{})
				go func() {
					defer close(reconcileDone)
					reconcileMastodonPeriodically(syncCtx, cfg.Mastodon.ReconcileInterval, client, coordinator, scope, cfg.Mastodon.ListIDs, targets, health)
				}()
				s := mastodon.NewSyncer(mastodon.SyncOptions{Scope: scope, API: mastodon.ClientTimelineAPI(client), Store: store, MasterSeed: seed, Targets: targets.Get, ListIDs: cfg.Mastodon.ListIDs, BackfillLimit: cfg.Mastodon.BackfillLimit, OutboxLimit: int64(cfg.Shared.OutboxLimit), StreamURL: mastodonStreamURL(cfg.Mastodon.BaseURL, token.AccessToken), Observer: mastodonHealthObserver{health}})
				reportMastodonSyncFailure(s.Run(syncCtx))
				cancelSync()
				<-reconcileDone
			}
		} else {
			reportMastodonFailure("authentication", err)
			health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) { m.AuthorizationAvailable = false; m.StreamConnected = false })
		}
		if !waitRuntimeRetry(ctx) {
			return
		}
	}
}

type neutralLiveTargets struct {
	mu     sync.RWMutex
	values neutral.IdentitySet
}

func newNeutralLiveTargets(values neutral.IdentitySet) *neutralLiveTargets {
	t := &neutralLiveTargets{}
	t.Set(values)
	return t
}
func (t *neutralLiveTargets) Set(values neutral.IdentitySet) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.values = make(neutral.IdentitySet, len(values))
	for identity := range values {
		t.values[identity] = struct{}{}
	}
}
func (t *neutralLiveTargets) Get() neutral.IdentitySet {
	t.mu.RLock()
	defer t.mu.RUnlock()
	values := make(neutral.IdentitySet, len(t.values))
	for identity := range t.values {
		values[identity] = struct{}{}
	}
	return values
}

func reconcileMastodonPeriodically(ctx context.Context, interval time.Duration, client *mastodon.Client, coordinator reconciliationCoordinator, scope bridgestore.SourceScope, listIDs []string, targets *neutralLiveTargets, health *Health) {
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) { m.PendingWork++ })
			snapshot, profiles, err := mastodon.NewReconciler(client, listIDs).Reconcile(ctx)
			if err != nil {
				reportMastodonFailure("periodic reconciliation", err)
			} else {
				err = coordinator.Reconcile(ctx, scope, snapshot, profiles)
				if err != nil {
					reportMastodonFailure("periodic publication", err)
				}
			}
			health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) {
				if m.PendingWork > 0 {
					m.PendingWork--
				}
				if err == nil {
					m.TargetCount = len(snapshot.Union)
					m.LastReconciliation = time.Now()
				}
			})
			if err == nil {
				targets.Set(snapshot.Union)
			}
		}
	}
}

type mastodonHealthObserver struct{ health *Health }

func (o mastodonHealthObserver) StreamConnected(connected bool) {
	o.health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) { m.StreamConnected = connected })
}
func (o mastodonHealthObserver) StreamEvent(at time.Time) {
	o.health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) { m.LastEvent = at })
}

func mastodonStreamURL(raw, accessToken string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/api/v1/streaming"
	u.RawQuery = url.Values{"access_token": {accessToken}}.Encode()
	return u.String()
}

type syncTargetLoader interface {
	SyncTargets(context.Context, bridgestore.SourceScope) ([]string, error)
}

func resolveInitialTargets(ctx context.Context, store syncTargetLoader, scope bridgestore.SourceScope, targets bluesky.TargetSet, reconcileErr error, health *Health) bluesky.TargetSet {
	if reconcileErr != nil {
		if persisted, loadErr := store.SyncTargets(ctx, scope); loadErr == nil {
			targets.Union = make(bluesky.DIDSet, len(persisted))
			for _, did := range persisted {
				targets.Union[did] = struct{}{}
			}
		}
	}
	health.Update(func(metrics *HealthMetrics) { metrics.TargetDIDCount = len(targets.Union) })
	health.UpdateProvider("bluesky", func(metrics *ProviderHealthMetrics) { metrics.TargetCount = len(targets.Union) })
	return targets
}

type healthSyncObserver struct{ health *Health }

func (o healthSyncObserver) JetstreamConnected(connected bool) {
	o.health.Update(func(m *HealthMetrics) { m.JetstreamConnected = connected })
	o.health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) { m.StreamConnected = connected })
}
func (o healthSyncObserver) JetstreamEvent(at time.Time) {
	o.health.Update(func(m *HealthMetrics) { m.LastJetstreamEvent = at })
	o.health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) { m.LastEvent = at })
}
func (o healthSyncObserver) SyncCompleted(at time.Time) {
	o.health.Update(func(m *HealthMetrics) { m.LastSync = at })
	o.health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) { m.LastReconciliation = at })
}
func (o healthSyncObserver) PendingWork(delta int) {
	o.health.Update(func(m *HealthMetrics) { m.PendingWork += delta })
	o.health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) {
		m.PendingWork += delta
		if m.PendingWork < 0 {
			m.PendingWork = 0
		}
	})
}

func reconcilePeriodically(ctx context.Context, interval time.Duration, source bluesky.SourceClient, coordinator reconciliationCoordinator, scope bridgestore.SourceScope, listURIs []string, store runtimeStore, outboxLimit int, live *liveTargets, health *Health) {
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var targets bluesky.TargetSet
			err := withProviderPending(health, "bluesky", func() error {
				var err error
				targets, err = bluesky.NewReconciler(source, listURIs).Reconcile(ctx)
				if err != nil {
					reportReconciliationFailure("periodic reconciliation", err)
					return err
				}
				err = applyPeriodicTargets(ctx, source, coordinator, scope, targets, store, outboxLimit, live, health)
				if err != nil {
					reportReconciliationFailure("periodic publication", err)
				}
				return err
			})
			if err != nil {
				continue
			}
		}
	}
}

func reportReconciliationFailure(operation string, err error) {
	reportRuntimeFailure(operation, err)
}

func reportSyncFailure(err error) {
	reportRuntimeFailure("sync", err)
}

func reportMastodonFailure(operation string, err error) {
	reportRuntimeFailure("mastodon "+operation, err)
}

func reportMastodonSyncFailure(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	reportMastodonFailure("sync", errors.New("stream synchronization failed"))
}

func reportRuntimeFailure(operation string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	log.Printf("nostr-bridge runtime: %s failed: %v", operation, err)
}

func applyPeriodicTargets(ctx context.Context, source bluesky.SourceClient, coordinator reconciliationCoordinator, scope bridgestore.SourceScope, targets bluesky.TargetSet, store runtimeStore, outboxLimit int, live *liveTargets, health *Health) error {
	if err := reconcileBluesky(ctx, coordinator, source, scope, targets); err != nil {
		return err
	}
	live.Set(targets.Union)
	health.Update(func(m *HealthMetrics) { m.TargetDIDCount = len(targets.Union) })
	health.UpdateProvider("bluesky", func(m *ProviderHealthMetrics) { m.TargetCount = len(targets.Union); m.LastReconciliation = time.Now() })
	return nil
}

type syncTargetStore interface {
	ReplaceSyncTargets(context.Context, bridgestore.SourceScope, []string) error
	SyncTargets(context.Context, bridgestore.SourceScope) ([]string, error)
}

func persistSyncTargets(ctx context.Context, store syncTargetStore, scope bridgestore.SourceScope, targets bluesky.DIDSet) error {
	dids := make([]string, 0, len(targets))
	for did := range targets {
		dids = append(dids, did)
	}
	sort.Strings(dids)
	return store.ReplaceSyncTargets(ctx, scope, dids)
}

func applyTargetReconciliation(ctx context.Context, store syncTargetStore, scope bridgestore.SourceScope, live *liveTargets, targets bluesky.DIDSet) error {
	if err := persistSyncTargets(ctx, store, scope, targets); err != nil {
		return err
	}
	live.Set(targets)
	return nil
}

type reconciliationCoordinator interface {
	Reconcile(context.Context, bridgestore.SourceScope, neutral.TargetSnapshot, []neutral.Profile) error
}

func reconcileBluesky(ctx context.Context, coordinator reconciliationCoordinator, source bluesky.SourceClient, scope bridgestore.SourceScope, targets bluesky.TargetSet) error {
	dids := make(bluesky.DIDSet, len(targets.Union)+1)
	for did := range targets.Union {
		dids[did] = struct{}{}
	}
	dids[scope.Account] = struct{}{}
	profiles := make([]neutral.Profile, 0, len(dids))
	for did := range dids {
		profile, err := source.Profile(ctx, did)
		if err != nil {
			return fmt.Errorf("read profile %s: %w", did, err)
		}
		profiles = append(profiles, neutral.Profile{Identity: neutral.ActorIdentity{Provider: "bluesky", ID: profile.DID}, DisplayName: profile.DisplayName, Description: profile.Description, AvatarURL: profile.Avatar, ProfileURL: "https://bsky.app/profile/" + profile.Handle})
	}
	return coordinator.Reconcile(ctx, scope, targets.Snapshot(), profiles)
}
