package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/bluesky"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/nostrmap"
	bridgeoauth "github.com/nakatanakatana/mytools/cmd/nostr-bridge/oauth"
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
	<-targets.updates
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

type runtimeStore interface {
	bridgestore.SyncDeliveryStore
	bridgestore.ReconciliationStore
}

func newRuntimeSync(cfg Config, store runtimeStore, oauthClient *bridgeoauth.Client, health *Health) (*runtimeSync, error) {
	seed, err := base64.StdEncoding.DecodeString(cfg.Shared.MasterSeed)
	if err != nil || len(seed) != 32 {
		return nil, errInvalidMasterSeed
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	runtime := &runtimeSync{cancel: cancel, done: done}
	go func() {
		defer close(done)
		runtime.run(ctx, cfg, seed, store, oauthClient, health)
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

func (r *runtimeSync) run(ctx context.Context, cfg Config, seed []byte, store runtimeStore, oauthClient *bridgeoauth.Client, health *Health) {
	for ctx.Err() == nil {
		token, err := oauthClient.TokenByAccountDID(ctx, cfg.Bluesky.AccountDID)
		if err == nil {
			if !token.Expiry.IsZero() && !token.Expiry.After(time.Now()) {
				health.Update(func(metrics *HealthMetrics) {
					metrics.OAuthConnected = false
					metrics.OAuthExpiry = token.Expiry
				})
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Second):
				}
				continue
			}
			health.Update(func(metrics *HealthMetrics) { metrics.OAuthConnected = true; metrics.OAuthExpiry = token.Expiry })
			if source, sourceErr := bluesky.NewClient(bluesky.ClientOptions{BaseURL: cfg.Bluesky.BaseURL, Token: token, AccountDID: cfg.Bluesky.AccountDID}); sourceErr == nil {
				health.Update(func(metrics *HealthMetrics) { metrics.PendingWork++ })
				targets, reconcileErr := bluesky.NewReconciler(source, cfg.Bluesky.ListURIs).Reconcile(ctx)
				if reconcileErr != nil {
					reportReconciliationFailure("initial reconciliation", reconcileErr)
				}
				targets = resolveInitialTargets(ctx, store, targets, reconcileErr, health)
				health.Update(func(metrics *HealthMetrics) {
					if metrics.PendingWork > 0 {
						metrics.PendingWork--
					}
				})
				if reconcileErr == nil {
					if err := publishReconciliation(ctx, source, seed, cfg.Bluesky.AccountDID, targets, store, cfg.Shared.OutboxLimit); err != nil {
						reportReconciliationFailure("initial publication", err)
						continue
					}
				}
				if len(targets.Union) > 0 {
					live := newLiveTargets(targets.Union)
					syncContext, stopSync := context.WithCancel(ctx)
					go reconcilePeriodically(syncContext, cfg.Bluesky.ReconcileInterval, source, seed, cfg.Bluesky.AccountDID, cfg.Bluesky.ListURIs, store, cfg.Shared.OutboxLimit, live, health)
					s := syncer.New(syncer.Options{Source: source, OutboxStore: store, OutboxLimit: int64(cfg.Shared.OutboxLimit), MasterSeed: seed, TargetProvider: live.Get, TargetUpdates: live.Updates(), BackfillLimit: cfg.Bluesky.BackfillLimit, JetstreamURL: cfg.Bluesky.JetstreamURL, Observer: healthSyncObserver{health}})
					reportSyncFailure(s.Run(syncContext))
					stopSync()
				}
			} else {
				reportRuntimeFailure("source construction", sourceErr)
			}
		} else {
			health.Update(func(metrics *HealthMetrics) { metrics.OAuthConnected = false; metrics.JetstreamConnected = false })
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

type syncTargetLoader interface {
	SyncTargets(context.Context) ([]string, error)
}

func resolveInitialTargets(ctx context.Context, store syncTargetLoader, targets bluesky.TargetSet, reconcileErr error, health *Health) bluesky.TargetSet {
	if reconcileErr != nil {
		if persisted, loadErr := store.SyncTargets(ctx); loadErr == nil {
			targets.Union = make(bluesky.DIDSet, len(persisted))
			for _, did := range persisted {
				targets.Union[did] = struct{}{}
			}
		}
	}
	health.Update(func(metrics *HealthMetrics) { metrics.TargetDIDCount = len(targets.Union) })
	return targets
}

type healthSyncObserver struct{ health *Health }

func (o healthSyncObserver) JetstreamConnected(connected bool) {
	o.health.Update(func(m *HealthMetrics) { m.JetstreamConnected = connected })
}
func (o healthSyncObserver) JetstreamEvent(at time.Time) {
	o.health.Update(func(m *HealthMetrics) { m.LastJetstreamEvent = at })
}
func (o healthSyncObserver) SyncCompleted(at time.Time) {
	o.health.Update(func(m *HealthMetrics) { m.LastSync = at })
}
func (o healthSyncObserver) PendingWork(delta int) {
	o.health.Update(func(m *HealthMetrics) { m.PendingWork += delta })
}

func reconcilePeriodically(ctx context.Context, interval time.Duration, source bluesky.SourceClient, seed []byte, accountDID string, listURIs []string, store runtimeStore, outboxLimit int, live *liveTargets, health *Health) {
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
			targets, err := bluesky.NewReconciler(source, listURIs).Reconcile(ctx)
			if err != nil {
				reportReconciliationFailure("periodic reconciliation", err)
				continue
			}
			if err := applyPeriodicTargets(ctx, source, seed, accountDID, targets, store, outboxLimit, live, health); err != nil {
				reportReconciliationFailure("periodic publication", err)
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

func reportRuntimeFailure(operation string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	log.Printf("nostr-bridge runtime: %s failed: %v", operation, err)
}

func applyPeriodicTargets(ctx context.Context, source bluesky.SourceClient, seed []byte, accountDID string, targets bluesky.TargetSet, store runtimeStore, outboxLimit int, live *liveTargets, health *Health) error {
	if err := publishReconciliation(ctx, source, seed, accountDID, targets, store, outboxLimit); err != nil {
		return err
	}
	live.Set(targets.Union)
	health.Update(func(m *HealthMetrics) { m.TargetDIDCount = len(targets.Union) })
	return nil
}

type syncTargetStore interface {
	ReplaceSyncTargets(context.Context, []string) error
	SyncTargets(context.Context) ([]string, error)
}

func persistSyncTargets(ctx context.Context, store syncTargetStore, targets bluesky.DIDSet) error {
	dids := make([]string, 0, len(targets))
	for did := range targets {
		dids = append(dids, did)
	}
	sort.Strings(dids)
	return store.ReplaceSyncTargets(ctx, dids)
}

func applyTargetReconciliation(ctx context.Context, store syncTargetStore, live *liveTargets, targets bluesky.DIDSet) error {
	if err := persistSyncTargets(ctx, store, targets); err != nil {
		return err
	}
	live.Set(targets)
	return nil
}

func publishReconciliation(ctx context.Context, source bluesky.SourceClient, seed []byte, accountDID string, targets bluesky.TargetSet, store bridgestore.ReconciliationStore, outboxLimit int) error {
	requests := make([]bridgestore.EventEnqueueRequest, 0)
	profiles := targets.Union
	profiles = appendDID(profiles, accountDID)
	for did := range profiles {
		profile, err := source.Profile(ctx, did)
		if err != nil {
			return fmt.Errorf("read profile %s: %w", did, err)
		}
		event, err := nostrmap.ProfileEvent(seed, neutral.Profile{Identity: blueskyActorIdentity(profile.DID), DisplayName: profile.DisplayName, Description: profile.Description, AvatarURL: profile.Avatar, ProfileURL: "https://bsky.app/profile/" + profile.Handle})
		if err != nil {
			return err
		}
		requests = append(requests, reconciliationEventRequest("at://"+did+"/app.bsky.actor.profile/self", event))
	}
	follows, err := nostrmap.FollowEvent(seed, blueskyActorIdentity(accountDID), blueskyIdentitySet(targets.Union))
	if err != nil {
		return err
	}
	requests = append(requests, reconciliationEventRequest("at://"+accountDID+"/app.bsky.graph.follow", follows))
	for uri, members := range targets.Lists {
		metadata := targets.ListMetadata[uri]
		identifier := metadata.URI
		if identifier == "" {
			identifier = uri
		}
		event, err := nostrmap.FollowSetEvent(seed, blueskyActorIdentity(accountDID), neutral.List{ID: identifier, Title: metadata.Name, Description: metadata.Description, Members: blueskyIdentitySet(members)})
		if err != nil {
			return err
		}
		requests = append(requests, reconciliationEventRequest(uri, event))
	}
	dids := make([]string, 0, len(targets.Union))
	for did := range targets.Union {
		dids = append(dids, did)
	}
	sort.Strings(dids)
	return store.Reconcile(ctx, bridgestore.ReconciliationRequest{Targets: dids, Events: requests, Limit: int64(outboxLimit)})
}

func blueskyActorIdentity(did string) neutral.ActorIdentity {
	return neutral.ActorIdentity{Provider: "bluesky", ID: did}
}

func blueskyIdentitySet(dids bluesky.DIDSet) neutral.IdentitySet {
	identities := make(neutral.IdentitySet, len(dids))
	for did := range dids {
		identities[blueskyActorIdentity(did)] = struct{}{}
	}
	return identities
}

func reconciliationEventRequest(sourceURI string, event nostr.Event) bridgestore.EventEnqueueRequest {
	payload, err := event.MarshalJSON()
	if err != nil {
		return bridgestore.EventEnqueueRequest{}
	}
	now := time.Now()
	identityPayload, _ := json.Marshal(struct {
		PubKey  string     `json:"pubkey"`
		Kind    nostr.Kind `json:"kind"`
		Tags    nostr.Tags `json:"tags"`
		Content string     `json:"content"`
	}{event.PubKey.Hex(), event.Kind, event.Tags, event.Content})
	identity := fmt.Sprintf("sha256:%x", sha256.Sum256(identityPayload))
	return bridgestore.EventEnqueueRequest{Mapping: bridgestore.EventMapping{SourceURI: sourceURI, NostrEventID: event.ID.Hex(), SourceKind: "reconciliation", AuthorPubKey: event.PubKey.Hex(), UpdatedAt: now.Unix()}, Event: bridgestore.OutboxRequest{AggregateKey: event.PubKey.Hex(), Operation: bridgestore.OutboxPublishEvent, PubKey: event.PubKey.Hex(), Payload: string(payload), AvailableAt: now}, SourceOperation: identity}
}
func appendDID(values bluesky.DIDSet, did string) bluesky.DIDSet {
	copied := make(bluesky.DIDSet, len(values)+1)
	for value := range values {
		copied[value] = struct{}{}
	}
	copied[did] = struct{}{}
	return copied
}
