package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/bluesky"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/nostrmap"
	bridgeowner "github.com/nakatanakatana/mytools/cmd/nostr-bridge/owner"
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
