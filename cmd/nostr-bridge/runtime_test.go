package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/bluesky"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/nostrmap"
	bridgeoauth "github.com/nakatanakatana/mytools/cmd/nostr-bridge/oauth"
	bridgeowner "github.com/nakatanakatana/mytools/cmd/nostr-bridge/owner"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

var runtimeTestScope = bridgestore.SourceScope{Provider: "bluesky", Account: "did:plc:owner"}

func TestRuntimeLogsReconciliationFailure(t *testing.T) {
	logs := captureRuntimeLogs(t)
	reportReconciliationFailure("initial reconciliation", errors.New("reconcile unavailable"))

	assertRuntimeLogContains(t, logs.String(), "initial reconciliation", "reconcile unavailable")
}

func TestRuntimeLogsPublicationFailure(t *testing.T) {
	logs := captureRuntimeLogs(t)
	reportReconciliationFailure("periodic publication", errors.New("outbox unavailable"))

	assertRuntimeLogContains(t, logs.String(), "periodic publication", "outbox unavailable")
}

func TestRuntimeLogsSyncFailure(t *testing.T) {
	logs := captureRuntimeLogs(t)
	reportSyncFailure(errors.New("jetstream unavailable"))

	assertRuntimeLogContains(t, logs.String(), "sync", "jetstream unavailable")
}

func TestRuntimeLogsSyncFailureSuppressesContextCanceled(t *testing.T) {
	logs := captureRuntimeLogs(t)
	reportSyncFailure(context.Canceled)

	if logs.Len() != 0 {
		t.Fatalf("log = %q, want empty", logs.String())
	}
}

func TestRuntimeLogsMastodonFailure(t *testing.T) {
	logs := captureRuntimeLogs(t)
	reportMastodonFailure("initial reconciliation", errors.New("instance unavailable"))

	assertRuntimeLogContains(t, logs.String(), "mastodon", "initial reconciliation", "instance unavailable")
}

func TestRuntimeLogsMastodonFailureSuppressesContextCanceled(t *testing.T) {
	logs := captureRuntimeLogs(t)
	reportMastodonFailure("sync", context.Canceled)

	if logs.Len() != 0 {
		t.Fatalf("log = %q, want empty", logs.String())
	}
}

func TestRuntimeLogsMastodonSyncFailureWithoutAccessToken(t *testing.T) {
	logs := captureRuntimeLogs(t)
	reportMastodonSyncFailure(errors.New("dial wss://social.example/api/v1/streaming?access_token=secret-token"))

	assertRuntimeLogContains(t, logs.String(), "mastodon", "sync", "stream synchronization failed")
	if strings.Contains(logs.String(), "secret-token") || strings.Contains(logs.String(), "access_token") {
		t.Fatalf("log contains streaming credentials: %q", logs.String())
	}
}

func captureRuntimeLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	return &logs
}

func assertRuntimeLogContains(t *testing.T, output string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(output, value) {
			t.Fatalf("log %q does not contain %q", output, value)
		}
	}
}

func TestRuntimeBlueskyReconciliationUsesSharedOwnerCoordinator(t *testing.T) {
	ctx := context.Background()
	s, closer, err := bridgestore.Open(ctx, filepath.Join(t.TempDir(), "reconciliation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	coordinator := bridgeowner.New(bridgeowner.Options{MasterSeed: []byte("seed"), OwnerID: "home", OwnerName: "Bridge", Store: s, OutboxLimit: 100})
	listURI := "at://did:plc:owner/app.bsky.graph.list/friends"
	targets := bluesky.TargetSet{Union: bluesky.DIDSet{"did:plc:alice": {}}, Lists: map[string]bluesky.DIDSet{listURI: {"did:plc:alice": {}}}, ListMetadata: map[string]bluesky.List{listURI: {URI: listURI, Name: "Friends"}}}
	if err := reconcileBluesky(ctx, coordinator, reconciliationSource{}, runtimeTestScope, targets); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []bridgestore.SourceRef{
		{Scope: bridgestore.SourceScope{Provider: "bridge-owner", Account: "home"}, URI: "owner/profile"},
		{Scope: bridgestore.SourceScope{Provider: "bridge-owner", Account: "home"}, URI: "owner/follows"},
		{Scope: runtimeTestScope, URI: "list/bluesky:" + listURI},
	} {
		if _, err := s.EventMappingBySourceURI(ctx, ref); err != nil {
			t.Fatalf("mapping %v: %v", ref, err)
		}
	}
	first, err := s.OutboxCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if err := reconcileBluesky(ctx, coordinator, reconciliationSource{}, runtimeTestScope, targets); err != nil {
		t.Fatal(err)
	}
	second, err := s.OutboxCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("identical reconciliation grew outbox: %d -> %d", first, second)
	}
	changed := targets
	changed.Union = bluesky.DIDSet{"did:plc:alice": {}, "did:plc:bob": {}}
	if err := reconcileBluesky(ctx, coordinator, reconciliationSource{}, runtimeTestScope, changed); err != nil {
		t.Fatal(err)
	}
	third, err := s.OutboxCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if third <= second {
		t.Fatalf("changed reconciliation did not enqueue update: %d -> %d", second, third)
	}
}

func TestRuntimeCoordinatorRestartPreservesUnavailableProviderSnapshotAndList(t *testing.T) {
	ctx := context.Background()
	s, closer, err := bridgestore.Open(ctx, filepath.Join(t.TempDir(), "restart.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	b := bridgestore.SourceScope{Provider: "bluesky", Account: "did:plc:owner"}
	m := bridgestore.SourceScope{Provider: "mastodon", Account: "owner@social.example"}
	scopes := []bridgestore.SourceScope{b, m}
	options := bridgeowner.Options{MasterSeed: []byte("01234567890123456789012345678901"), OwnerID: "home", Store: s, OutboxLimit: 100, EnabledScopes: scopes, Now: func() time.Time { return time.Unix(100, 0) }}
	first := bridgeowner.New(options)
	blue := source.TargetSnapshot{Union: source.IdentitySet{{Provider: "bluesky", ID: "did:plc:alice"}: {}}, Lists: map[string]source.List{}}
	bob := source.ActorIdentity{Provider: "mastodon", ID: "https://social.example/users/bob"}
	masto := source.TargetSnapshot{Union: source.IdentitySet{bob: {}}, Lists: map[string]source.List{"7": {ID: "7", Members: source.IdentitySet{bob: {}}}}}
	if err := first.Reconcile(ctx, b, blue, nil); err != nil {
		t.Fatal(err)
	}
	if err := first.Reconcile(ctx, m, masto, nil); err != nil {
		t.Fatal(err)
	}

	restarted := bridgeowner.New(options)
	blue.Union[source.ActorIdentity{Provider: "bluesky", ID: "did:plc:carol"}] = struct{}{}
	if err := restarted.Reconcile(ctx, b, blue, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EventMappingBySourceURI(ctx, bridgestore.SourceRef{Scope: m, URI: "list/mastodon:7"}); err != nil {
		t.Fatalf("unavailable provider list removed: %v", err)
	}
	var latest nostr.Event
	for i := 0; i < 30; i++ {
		items, err := s.ClaimOutbox(ctx, time.Now().Add(time.Hour), time.Minute, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			switch item.Operation {
			case bridgestore.OutboxAllowPublisher:
				if err := s.CompletePublisherRegistration(ctx, item.ID, item.ClaimToken, item.PubKey, time.Now()); err != nil {
					t.Fatal(err)
				}
			case bridgestore.OutboxPublishEvent:
				var event nostr.Event
				if event.UnmarshalJSON([]byte(item.Payload)) == nil && event.Kind == nostr.KindFollowList {
					latest = event
				}
				if err := s.CompleteOutbox(ctx, item.ID, item.ClaimToken, time.Now()); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unexpected operation %s", item.Operation)
			}
		}
	}
	for _, identity := range []source.ActorIdentity{{Provider: "bluesky", ID: "did:plc:alice"}, {Provider: "bluesky", ID: "did:plc:carol"}, bob} {
		key, _ := nostrmap.DeriveActorKey(options.MasterSeed, identity)
		if latest.Tags.FindWithValue("p", key.Public().Hex()) == nil {
			t.Errorf("latest owner follows missing %#v", identity)
		}
	}
	if latest.CreatedAt != 102 {
		t.Fatalf("latest owner follows created_at = %d, want 102", latest.CreatedAt)
	}
}

func TestEnabledProviderScopesMatchRuntimeProviderIdentities(t *testing.T) {
	cfg := Config{Bluesky: BlueskyConfig{BaseURL: "https://bsky.example", AccountDID: "did:plc:owner"}, Mastodon: MastodonConfig{BaseURL: "https://social.example", Account: "owner"}}
	want := []bridgestore.SourceScope{{Provider: "bluesky", Account: "did:plc:owner"}, {Provider: "mastodon", Account: "owner@social.example"}}
	if got := enabledProviderScopes(cfg); !slices.Equal(got, want) {
		t.Fatalf("scopes = %#v, want %#v", got, want)
	}
}

func TestApplyTargetReconciliationPersistsBeforeSwitchingLiveTargets(t *testing.T) {
	live := newLiveTargets(bluesky.DIDSet{"did:plc:old": {}})
	failed := failingTargetStore{}
	if err := applyTargetReconciliation(context.Background(), failed, runtimeTestScope, live, bluesky.DIDSet{"did:plc:new": {}}); err == nil {
		t.Fatal("expected persistence error")
	}
	if !live.Get().Has("did:plc:old") || live.Get().Has("did:plc:new") {
		t.Fatalf("live switched after failed persistence: %#v", live.Get())
	}
}

func TestInitialTargetMetricUsesPersistedTargetsAfterReconcileFailure(t *testing.T) {
	health := NewHealth(HealthOptions{})
	store := &periodicTargetStore{targets: []string{"did:plc:alice"}}
	targets := resolveInitialTargets(context.Background(), store, runtimeTestScope, bluesky.TargetSet{}, errors.New("reconcile failed"), health)
	if !targets.Union.Has("did:plc:alice") {
		t.Fatalf("resolved targets = %#v", targets.Union)
	}
	if got := health.snapshot().TargetDIDCount; got != 1 {
		t.Fatalf("TargetDIDCount = %d", got)
	}
}

func TestBlueskyObserverUpdatesProviderPendingAndSync(t *testing.T) {
	health := NewHealth(HealthOptions{EnabledProviders: []string{"bluesky"}})
	o := healthSyncObserver{health}
	now := time.Unix(42, 0)
	o.PendingWork(1)
	if got := health.providerSnapshot("bluesky").PendingWork; got != 1 {
		t.Fatalf("pending = %d", got)
	}
	o.SyncCompleted(now)
	if got := health.providerSnapshot("bluesky").LastReconciliation; !got.Equal(now) {
		t.Fatalf("last reconciliation = %v", got)
	}
	o.PendingWork(-1)
	if got := health.providerSnapshot("bluesky").PendingWork; got != 0 {
		t.Fatalf("pending = %d", got)
	}
}

func TestApplyPeriodicTargetsUpdatesBlueskyProviderHealth(t *testing.T) {
	health := NewHealth(HealthOptions{EnabledProviders: []string{"bluesky"}})
	live := newLiveTargets(bluesky.DIDSet{})
	targets := bluesky.TargetSet{Union: bluesky.DIDSet{"did:plc:alice": {}}}
	if err := applyPeriodicTargets(context.Background(), reconciliationSource{}, successfulCoordinator{}, runtimeTestScope, targets, nil, 100, live, health); err != nil {
		t.Fatal(err)
	}
	m := health.providerSnapshot("bluesky")
	if m.TargetCount != 1 || m.LastReconciliation.IsZero() {
		t.Fatalf("provider health = %#v", m)
	}
	first := m.LastReconciliation
	time.Sleep(time.Millisecond)
	targets.Union["did:plc:bob"] = struct{}{}
	if err := applyPeriodicTargets(context.Background(), reconciliationSource{}, successfulCoordinator{}, runtimeTestScope, targets, nil, 100, live, health); err != nil {
		t.Fatal(err)
	}
	m = health.providerSnapshot("bluesky")
	if m.TargetCount != 2 || !m.LastReconciliation.After(first) {
		t.Fatalf("second provider health = %#v", m)
	}
}

func TestNewLiveTargetsReturnsForEmptyInitialSet(t *testing.T) {
	done := make(chan struct{})
	go func() {
		_ = newLiveTargets(bluesky.DIDSet{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("newLiveTargets blocked for an empty initial set")
	}
}

func TestProviderPendingAlwaysDecrementsAfterFailure(t *testing.T) {
	health := NewHealth(HealthOptions{EnabledProviders: []string{"mastodon"}})
	for range 3 {
		_ = withProviderPending(health, "mastodon", func() error { return errors.New("construct failed") })
	}
	if got := health.providerSnapshot("mastodon").PendingWork; got != 0 {
		t.Fatalf("pending = %d", got)
	}
}

func TestBlueskyPeriodicReconciliationReportsPendingAndJoinsOnCancellation(t *testing.T) {
	health := NewHealth(HealthOptions{EnabledProviders: []string{"bluesky"}})
	source := &blockingReconciliationSource{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconcilePeriodically(ctx, time.Millisecond, source, successfulCoordinator{}, runtimeTestScope, nil, nil, 100, newLiveTargets(bluesky.DIDSet{"did:plc:old": {}}), health)
	}()
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("reconciliation did not start")
	}
	if got := health.providerSnapshot("bluesky").PendingWork; got != 1 {
		t.Fatalf("pending during blocked work = %d", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("periodic reconciliation did not join")
	}
	if got := health.providerSnapshot("bluesky").PendingWork; got != 0 {
		t.Fatalf("pending after cancellation = %d", got)
	}
}

func TestRuntimeRetryDelayIsBoundedAndCancellationAware(t *testing.T) {
	old := runtimeRetryDelay
	runtimeRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() { runtimeRetryDelay = old })
	started := time.Now()
	if !waitRuntimeRetry(context.Background()) {
		t.Fatal("delay canceled")
	}
	if time.Since(started) < 15*time.Millisecond {
		t.Fatal("retry delay tight-looped")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started = time.Now()
	if waitRuntimeRetry(ctx) {
		t.Fatal("canceled delay continued")
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("cancellation was not prompt")
	}
}

type blockingReconciliationSource struct {
	started chan struct{}
	once    sync.Once
}

func (s *blockingReconciliationSource) Timeline(context.Context, string, int) (bluesky.Page, error) {
	return bluesky.Page{}, nil
}
func (s *blockingReconciliationSource) Follows(ctx context.Context) ([]bluesky.Actor, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s *blockingReconciliationSource) List(context.Context, string) (bluesky.List, error) {
	return bluesky.List{}, nil
}
func (s *blockingReconciliationSource) Profile(context.Context, string) (bluesky.Profile, error) {
	return bluesky.Profile{}, nil
}

type successfulCoordinator struct{}

func (successfulCoordinator) Reconcile(context.Context, bridgestore.SourceScope, source.TargetSnapshot, []source.Profile) error {
	return nil
}

func TestTargetRemovalRetainsPublisherMappingAndOutboxAndReadditionKey(t *testing.T) {
	ctx := context.Background()
	s, closer, err := bridgestore.Open(ctx, filepath.Join(t.TempDir(), "targets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	did := "did:plc:alice"
	key, _ := nostrmap.DeriveKey([]byte("seed"), did)
	pubkey := key.Public().Hex()
	event := nostr.Event{CreatedAt: nostr.Now(), Kind: 1, Content: "retained"}
	if err := event.Sign(key); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPublisherRegistered(ctx, pubkey, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEventAndEnqueue(ctx, bridgestore.EventMapping{Source: bridgestore.SourceRef{Scope: runtimeTestScope, URI: "source"}, NostrEventID: event.ID.Hex(), AuthorPubKey: pubkey}, bridgestore.OutboxRequest{AggregateKey: pubkey, Operation: bridgestore.OutboxPublishEvent, PubKey: pubkey, Payload: event.String(), AvailableAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	live := newLiveTargets(bluesky.DIDSet{did: {}})
	if err := applyTargetReconciliation(ctx, s, runtimeTestScope, live, bluesky.DIDSet{}); err != nil {
		t.Fatal(err)
	}
	if targets, err := s.SyncTargets(ctx, runtimeTestScope); err != nil || len(targets) != 0 {
		t.Fatalf("targets = %#v, %v", targets, err)
	}
	if live.Get().Has(did) {
		t.Fatal("removed DID remained live")
	}
	if registered, _ := s.PublisherRegistered(ctx, pubkey); !registered {
		t.Fatal("registration removed")
	}
	if _, err := s.EventMappingBySourceURI(ctx, bridgestore.SourceRef{Scope: runtimeTestScope, URI: "source"}); err != nil {
		t.Fatal("mapping removed")
	}
	if count, _ := s.OutboxCount(ctx); count != 1 {
		t.Fatalf("outbox count = %d", count)
	}
	if err := applyTargetReconciliation(ctx, s, runtimeTestScope, live, bluesky.DIDSet{did: {}}); err != nil {
		t.Fatal(err)
	}
	readded, _ := nostrmap.DeriveKey([]byte("seed"), did)
	if readded.Public() != key.Public() {
		t.Fatal("readdition changed deterministic publisher key")
	}
}

type periodicTargetStore struct {
	targets []string
	fail    bool
}

func (s *periodicTargetStore) ReplaceSyncTargets(_ context.Context, _ bridgestore.SourceScope, targets []string) error {
	if s.fail {
		return errors.New("persist failed")
	}
	s.targets = append([]string(nil), targets...)
	return nil
}
func (s *periodicTargetStore) SyncTargets(context.Context, bridgestore.SourceScope) ([]string, error) {
	return append([]string(nil), s.targets...), nil
}

type failingTargetStore struct{}

func (failingTargetStore) ReplaceSyncTargets(context.Context, bridgestore.SourceScope, []string) error {
	return errors.New("persist failed")
}
func (failingTargetStore) SyncTargets(context.Context, bridgestore.SourceScope) ([]string, error) {
	return nil, nil
}

type fakeBlueskyTokenProvider struct {
	token       bridgeoauth.Token
	err         error
	status      bridgeoauth.Status
	statusErr   error
	statusCalls int
}

func (f *fakeBlueskyTokenProvider) TokenByAccountDID(context.Context, string) (bridgeoauth.Token, error) {
	return f.token, f.err
}

func (f *fakeBlueskyTokenProvider) AuthorizationStatus(context.Context, string, time.Duration) (bridgeoauth.Status, error) {
	f.statusCalls++
	return f.status, f.statusErr
}

func TestHealthBlueskyTokenProvider(t *testing.T) {
	health := NewHealth(HealthOptions{EnabledProviders: []string{"bluesky", "mastodon"}})
	mastoExpiry := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	health.UpdateProvider("mastodon", func(m *ProviderHealthMetrics) {
		m.AuthorizationAvailable = true
		m.OAuthExpiry = mastoExpiry
	})

	futureTime := time.Now().Add(time.Hour).Truncate(time.Second)
	fake := &fakeBlueskyTokenProvider{
		token:  bridgeoauth.Token{AccessToken: "token123", Expiry: futureTime},
		status: bridgeoauth.Status{AccessTokenValid: true, AuthorizationAvailable: true},
	}

	provider := healthBlueskyTokenProvider{tokens: fake, health: health, refreshPeriod: 30 * 24 * time.Hour}

	gotToken, err := provider.TokenByAccountDID(context.Background(), "did:plc:alice")
	if err != nil {
		t.Fatalf("TokenByAccountDID() error = %v", err)
	}
	if gotToken.AccessToken != "token123" {
		t.Fatalf("TokenByAccountDID() got AccessToken %q, want %q", gotToken.AccessToken, "token123")
	}

	globalSnap := health.snapshot()
	if !globalSnap.OAuthConnected {
		t.Error("global OAuthConnected = false, want true")
	}
	if !globalSnap.OAuthExpiry.Equal(futureTime) {
		t.Errorf("global OAuthExpiry = %v, want %v", globalSnap.OAuthExpiry, futureTime)
	}

	bskySnap := health.providerSnapshot("bluesky")
	if !bskySnap.AuthorizationAvailable {
		t.Error("bluesky AuthorizationAvailable = false, want true")
	}
	if !bskySnap.OAuthExpiry.Equal(futureTime) {
		t.Errorf("bluesky OAuthExpiry = %v, want %v", bskySnap.OAuthExpiry, futureTime)
	}

	// A transient token refresh failure remains authorization-available according
	// to the durable local status.
	fake.err = errors.New("refresh failed")
	fake.token = bridgeoauth.Token{}
	fake.status = bridgeoauth.Status{
		AuthorizationAvailable: true,
		LastRefreshErrorClass:  bridgeoauth.RefreshErrorServer,
	}

	_, err = provider.TokenByAccountDID(context.Background(), "did:plc:alice")
	if err == nil {
		t.Fatal("TokenByAccountDID() expected error, got nil")
	}

	bskySnap = health.providerSnapshot("bluesky")
	if !bskySnap.AuthorizationAvailable || !bskySnap.Degraded {
		t.Errorf("bluesky authorization after transient error = %#v", bskySnap)
	}
	if fake.statusCalls != 2 {
		t.Errorf("AuthorizationStatus calls = %d, want 2 after token success and failure", fake.statusCalls)
	}

	fake.status = bridgeoauth.Status{
		ReauthRequired:        true,
		LastRefreshErrorClass: bridgeoauth.RefreshErrorInvalidGrant,
	}
	_, _ = provider.TokenByAccountDID(context.Background(), "did:plc:alice")
	bskySnap = health.providerSnapshot("bluesky")
	if bskySnap.AuthorizationAvailable || !bskySnap.ReauthRequired {
		t.Errorf("bluesky authorization after permanent error = %#v", bskySnap)
	}

	mastoSnap := health.providerSnapshot("mastodon")
	if !mastoSnap.AuthorizationAvailable {
		t.Error("mastodon AuthorizationAvailable = false, want true (should be unchanged)")
	}
	if !mastoSnap.OAuthExpiry.Equal(mastoExpiry) {
		t.Errorf("mastodon OAuthExpiry = %v, want %v (should be unchanged)", mastoSnap.OAuthExpiry, mastoExpiry)
	}
}

func TestHealthBlueskyOAuthMaintenanceObserverUpdatesCachedStatusAndCounters(t *testing.T) {
	health := NewHealth(HealthOptions{EnabledProviders: []string{"bluesky"}})
	observer := healthOAuthMaintenanceObserver{health: health}
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	lastSuccess := now.Add(-time.Hour)
	nextRefresh := now.Add(30 * 24 * time.Hour)
	expiry := now.Add(time.Hour)

	observer.Started(now)
	observer.Checked(now, bridgeoauth.Status{
		AccessTokenValid:       false,
		AccessTokenExpiry:      expiry,
		AuthorizationAvailable: true,
		LastRefreshSucceededAt: lastSuccess,
		NextMaintenanceRefresh: nextRefresh,
		LastRefreshErrorClass:  bridgeoauth.RefreshErrorServer,
	})
	observer.RefreshSucceeded(now, bridgeoauth.RefreshReasonOnDemand)
	observer.RefreshFailed(now, bridgeoauth.RefreshReasonMaintenance, bridgeoauth.RefreshErrorServer)
	observer.RetryScheduled(now, bridgeoauth.RefreshReasonMaintenance, bridgeoauth.RefreshErrorServer, time.Second)

	got := health.providerSnapshot("bluesky")
	if !got.MaintenanceWorkerRunning || !got.AuthorizationAvailable || !got.AccessTokenExpired || !got.Degraded {
		t.Errorf("cached authorization status = %#v", got)
	}
	if got.ReauthRequired {
		t.Errorf("cached authorization unexpectedly requires reauthorization: %#v", got)
	}
	if !got.LastRefreshSucceededAt.Equal(lastSuccess) || !got.NextMaintenanceRefresh.Equal(nextRefresh) {
		t.Errorf("cached refresh timestamps = %#v", got)
	}
	if !got.OAuthExpiry.Equal(expiry) || !health.snapshot().OAuthExpiry.Equal(expiry) {
		t.Errorf("cached OAuth expiry: provider=%v global=%v, want %v", got.OAuthExpiry, health.snapshot().OAuthExpiry, expiry)
	}
	if got.LastRefreshErrorClass != bridgeoauth.RefreshErrorServer {
		t.Errorf("last refresh error class = %q", got.LastRefreshErrorClass)
	}
	if got.RefreshSuccesses[bridgeoauth.RefreshReasonOnDemand] != 1 {
		t.Errorf("on-demand successes = %d", got.RefreshSuccesses[bridgeoauth.RefreshReasonOnDemand])
	}
	if got.RefreshFailures[bridgeoauth.RefreshReasonMaintenance][bridgeoauth.RefreshErrorServer] != 1 {
		t.Errorf("maintenance server failures = %d", got.RefreshFailures[bridgeoauth.RefreshReasonMaintenance][bridgeoauth.RefreshErrorServer])
	}
	if got.RefreshExecutions[bridgeoauth.RefreshReasonOnDemand] != 1 ||
		got.RefreshExecutions[bridgeoauth.RefreshReasonMaintenance] != 1 {
		t.Errorf("refresh executions = %#v", got.RefreshExecutions)
	}

	observer.Stopped(now)
	if health.providerSnapshot("bluesky").MaintenanceWorkerRunning {
		t.Fatal("maintenance worker remained running after stop")
	}
}

func TestHealthBlueskyOAuthMaintenanceObserverBoundsUnknownLabels(t *testing.T) {
	health := NewHealth(HealthOptions{EnabledProviders: []string{"bluesky"}})
	observer := healthOAuthMaintenanceObserver{health: health}

	observer.RefreshSucceeded(time.Time{}, bridgeoauth.RefreshReason("did:plc:fixture-secret"))
	observer.RefreshFailed(
		time.Time{},
		bridgeoauth.RefreshReasonMaintenance,
		bridgeoauth.RefreshErrorClass("access-secret"),
	)

	got := health.providerSnapshot("bluesky")
	if len(got.RefreshSuccesses) != 0 || len(got.RefreshExecutions) != 1 {
		t.Errorf("unknown reason affected counters: successes=%#v executions=%#v", got.RefreshSuccesses, got.RefreshExecutions)
	}
	if got.RefreshFailures[bridgeoauth.RefreshReasonMaintenance][bridgeoauth.RefreshErrorProtocol] != 1 {
		t.Errorf("unknown class was not bounded to protocol: %#v", got.RefreshFailures)
	}
}

type lifecycleOAuthMaintenanceClient struct {
	status      bridgeoauth.Status
	statusCalls int
}

func (c *lifecycleOAuthMaintenanceClient) RefreshIfDue(ctx context.Context, _ string, _ time.Duration) (bridgeoauth.RefreshResult, error) {
	<-ctx.Done()
	return bridgeoauth.RefreshResult{}, nil
}

func (c *lifecycleOAuthMaintenanceClient) AuthorizationStatus(context.Context, string, time.Duration) (bridgeoauth.Status, error) {
	c.statusCalls++
	return c.status, nil
}

func TestBlueskyOAuthMaintenanceRestoresAuthorizationBeforeRunning(t *testing.T) {
	health := NewHealth(HealthOptions{EnabledProviders: []string{"bluesky"}})
	status := bridgeoauth.Status{
		AccessTokenValid:       false,
		AuthorizationAvailable: true,
		LastRefreshSucceededAt: time.Unix(100, 0),
		NextMaintenanceRefresh: time.Unix(200, 0),
		LastRefreshErrorClass:  bridgeoauth.RefreshErrorServer,
	}
	client := &lifecycleOAuthMaintenanceClient{status: status}

	if health.providerSnapshot("bluesky").MaintenanceWorkerRunning {
		t.Fatal("maintenance worker marked running before launch")
	}
	worker := startBlueskyOAuthMaintenance(BlueskyConfig{
		AccountDID:                "did:plc:owner",
		OAuthRefreshPeriod:        time.Hour,
		OAuthRefreshCheckInterval: time.Hour,
	}, client, health)
	t.Cleanup(func() {
		worker.Cancel()
		_ = worker.Wait(context.Background())
	})

	if client.statusCalls != 1 {
		t.Fatalf("startup AuthorizationStatus calls = %d, want 1", client.statusCalls)
	}
	restored := health.providerSnapshot("bluesky")
	if !restored.AuthorizationAvailable || !restored.AccessTokenExpired || !restored.Degraded {
		t.Fatalf("restored authorization = %#v", restored)
	}
	if !restored.LastRefreshSucceededAt.Equal(status.LastRefreshSucceededAt) ||
		!restored.NextMaintenanceRefresh.Equal(status.NextMaintenanceRefresh) {
		t.Fatalf("restored refresh timestamps = %#v", restored)
	}

	deadline := time.Now().Add(time.Second)
	for !health.providerSnapshot("bluesky").MaintenanceWorkerRunning && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !health.providerSnapshot("bluesky").MaintenanceWorkerRunning {
		t.Fatal("maintenance worker was not marked running after launch")
	}

	worker.Cancel()
	if err := worker.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if health.providerSnapshot("bluesky").MaintenanceWorkerRunning {
		t.Fatal("maintenance worker remained running after Run returned")
	}
}

func TestOAuthMaintenanceLogsUseOnlyBoundedFields(t *testing.T) {
	logs := captureRuntimeLogs(t)
	observer := healthOAuthMaintenanceObserver{
		health: NewHealth(HealthOptions{EnabledProviders: []string{"bluesky"}}),
	}
	secrets := []string{
		"did:plc:fixture-secret",
		"access-token-fixture",
		"refresh-token-fixture",
		"authorization-code-fixture",
		"dpop-nonce-fixture",
		`{"d":"private-jwk-fixture"}`,
		"encrypted-payload-fixture",
		"remote-response-description-fixture",
	}

	observer.Checked(time.Time{}, bridgeoauth.Status{
		LastRefreshErrorClass: bridgeoauth.RefreshErrorClass(strings.Join(secrets, "|")),
	})
	observer.RefreshFailed(time.Time{}, bridgeoauth.RefreshReasonMaintenance, bridgeoauth.RefreshErrorServer)
	observer.RetryScheduled(time.Time{}, bridgeoauth.RefreshReasonMaintenance, bridgeoauth.RefreshErrorServer, time.Second)
	observer.RefreshFailed(time.Time{}, bridgeoauth.RefreshReasonMaintenance, bridgeoauth.RefreshErrorInvalidGrant)
	observer.RefreshFailed(
		time.Time{},
		bridgeoauth.RefreshReason(strings.Join(secrets, "|")),
		bridgeoauth.RefreshErrorClass(strings.Join(secrets, "|")),
	)

	output := logs.String()
	assertRuntimeLogContains(
		t,
		output,
		"provider=bluesky reason=maintenance result=checked class=protocol retry=false",
		"provider=bluesky reason=maintenance result=failed class=server retry=false",
		"provider=bluesky reason=maintenance result=retry_scheduled class=server retry=true",
		"provider=bluesky reason=maintenance result=failed class=invalid_grant retry=false",
	)
	for _, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Fatalf("OAuth maintenance log contains secret %q: %q", secret, output)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		const marker = "nostr-bridge OAuth maintenance: "
		index := strings.Index(line, marker)
		if index < 0 {
			t.Fatalf("unexpected OAuth maintenance log line: %q", line)
		}
		fields := strings.Fields(line[index+len(marker):])
		if len(fields) != 5 {
			t.Fatalf("OAuth maintenance log fields = %v, want five bounded fields", fields)
		}
		for index, prefix := range []string{"provider=", "reason=", "result=", "class=", "retry="} {
			if !strings.HasPrefix(fields[index], prefix) {
				t.Fatalf("OAuth maintenance log fields = %v, field %d want prefix %q", fields, index, prefix)
			}
		}
	}
}
