package owner

import (
	"context"
	"errors"
	"sync"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/nostrmap"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

func TestCoordinatorPublishesAggregateFollowFromLatestSnapshots(t *testing.T) {
	s := &recordingStore{}
	c := New(Options{MasterSeed: []byte("01234567890123456789012345678901"), OwnerID: "home", Store: s, OutboxLimit: 100})
	b := store.SourceScope{Provider: "bluesky", Account: "did:plc:owner"}
	m := store.SourceScope{Provider: "mastodon", Account: "owner"}
	if err := c.Reconcile(context.Background(), b, snapshot(identity("bluesky", "did:plc:alice")), nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(context.Background(), m, snapshot(identity("mastodon", "https://social.example/users/bob")), nil); err != nil {
		t.Fatal(err)
	}
	e := s.latestKind(t, nostr.KindFollowList)
	assertFollows(t, e, []source.ActorIdentity{identity("bluesky", "did:plc:alice"), identity("mastodon", "https://social.example/users/bob")})
}

func TestCoordinatorPreservesLastGoodSnapshotAfterFailedPass(t *testing.T) {
	s := &recordingStore{}
	c := New(Options{MasterSeed: []byte("01234567890123456789012345678901"), OwnerID: "home", Store: s, OutboxLimit: 100})
	b := store.SourceScope{Provider: "bluesky", Account: "owner"}
	m := store.SourceScope{Provider: "mastodon", Account: "owner"}
	alice, carol := identity("bluesky", "alice"), identity("bluesky", "carol")
	if err := c.Reconcile(context.Background(), b, snapshot(alice), nil); err != nil {
		t.Fatal(err)
	}
	s.failAt = 2
	if err := c.Reconcile(context.Background(), b, snapshot(carol), nil); err == nil {
		t.Fatal("expected failure")
	}
	s.failAt = 0
	if err := c.Reconcile(context.Background(), m, snapshot(identity("mastodon", "bob")), nil); err != nil {
		t.Fatal(err)
	}
	e := s.latestKind(t, nostr.KindFollowList)
	assertFollows(t, e, []source.ActorIdentity{alice, identity("mastodon", "bob")})
}

func TestCoordinatorHydratesEveryEnabledProviderBeforePublishingAggregate(t *testing.T) {
	b := store.SourceScope{Provider: "bluesky", Account: "did:plc:owner"}
	m := store.SourceScope{Provider: "mastodon", Account: "owner@example.com"}
	s := &recordingStore{targets: map[store.SourceScope][]string{b: {"did:plc:old"}, m: {"https://social.example/users/bob"}}}
	c := New(Options{MasterSeed: []byte("01234567890123456789012345678901"), OwnerID: "home", Store: s, OutboxLimit: 100, EnabledScopes: []store.SourceScope{b, m}})

	if err := c.Reconcile(context.Background(), b, snapshot(identity("bluesky", "did:plc:new")), nil); err != nil {
		t.Fatal(err)
	}
	assertFollows(t, s.latestKind(t, nostr.KindFollowList), []source.ActorIdentity{
		identity("bluesky", "did:plc:new"),
		identity("mastodon", "https://social.example/users/bob"),
	})
}

func TestCoordinatorHydrationFailureDoesNotPublishPartialAggregate(t *testing.T) {
	b := store.SourceScope{Provider: "bluesky", Account: "did:plc:owner"}
	m := store.SourceScope{Provider: "mastodon", Account: "owner@example.com"}
	s := &recordingStore{loadErr: errors.New("read persisted targets")}
	c := New(Options{MasterSeed: []byte("01234567890123456789012345678901"), OwnerID: "home", Store: s, OutboxLimit: 100, EnabledScopes: []store.SourceScope{b, m}})

	if err := c.Reconcile(context.Background(), b, snapshot(identity("bluesky", "did:plc:new")), nil); err == nil {
		t.Fatal("expected hydration error")
	}
	if len(s.requests) != 0 {
		t.Fatalf("published %d destructive partial batches", len(s.requests))
	}
}

func TestCoordinatorUsesStableOwnerAndProviderScopes(t *testing.T) {
	s := &recordingStore{}
	c := New(Options{MasterSeed: []byte("01234567890123456789012345678901"), OwnerID: "home", Store: s, OutboxLimit: 100})
	b := store.SourceScope{Provider: "bluesky", Account: "alice"}
	m := store.SourceScope{Provider: "mastodon", Account: "bob"}
	blue := snapshot(identity("bluesky", "blue"))
	blue.Lists["friends"] = source.List{ID: "friends", Members: blue.Union}
	if err := c.Reconcile(context.Background(), b, blue, []source.Profile{{Identity: identity("bluesky", "blue")}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(context.Background(), m, snapshot(identity("mastodon", "red")), nil); err != nil {
		t.Fatal(err)
	}
	ownerScope := store.SourceScope{Provider: "bridge-owner", Account: "home"}
	s.assertEventScope(t, nostr.KindProfileMetadata, ownerScope, "owner/profile")
	s.assertEventScope(t, nostr.KindFollowList, ownerScope, "owner/follows")
	s.assertEventScope(t, nostr.Kind(30000), b, "list/bluesky:friends")
	s.assertEventScope(t, nostr.KindProfileMetadata, b, "profile/bluesky:blue")
}

func TestCoordinatorPublishesOwnerProfileAndQualifiedListID(t *testing.T) {
	s := &recordingStore{}
	c := New(Options{MasterSeed: []byte("01234567890123456789012345678901"), OwnerID: "home", OwnerName: "Bridge", Store: s, OutboxLimit: 100})
	snap := snapshot(identity("bluesky", "alice"))
	snap.Lists = map[string]source.List{"friends": {ID: "friends", Members: snap.Union}}
	if err := c.Reconcile(context.Background(), store.SourceScope{Provider: "bluesky", Account: "owner"}, snap, nil); err != nil {
		t.Fatal(err)
	}
	profile := s.latestKind(t, nostr.KindProfileMetadata)
	ownerKey, _ := nostrmap.DeriveActorKey([]byte("01234567890123456789012345678901"), identity("bridge-owner", "home"))
	if profile.PubKey != ownerKey.Public() {
		t.Fatalf("owner profile pubkey = %s", profile.PubKey.Hex())
	}
	list := s.latestKind(t, nostr.Kind(30000))
	if tag := list.Tags.Find("d"); len(tag) < 2 || tag[1] != "bluesky:friends" {
		t.Fatalf("d tag = %#v", tag)
	}
}

type recordingStore struct {
	mu       sync.Mutex
	requests []store.ReconciliationRequest
	failAt   int
	calls    int
	targets  map[store.SourceScope][]string
	loadErr  error
}

func (s *recordingStore) SyncTargets(_ context.Context, scope store.SourceScope) ([]string, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return append([]string(nil), s.targets[scope]...), nil
}
func (s *recordingStore) Reconcile(_ context.Context, r store.ReconciliationRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.failAt == s.calls {
		return errors.New("failed")
	}
	s.requests = append(s.requests, r)
	return nil
}
func (s *recordingStore) ReconcileBatch(_ context.Context, r store.ReconciliationBatchRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.failAt == s.calls {
		return errors.New("failed")
	}
	s.requests = append(s.requests, store.ReconciliationRequest{Scope: r.TargetScope, Targets: r.Targets, Events: r.Events, Limit: r.Limit})
	return nil
}
func (s *recordingStore) latestKind(t *testing.T, kind nostr.Kind) nostr.Event {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.requests) - 1; i >= 0; i-- {
		for _, req := range s.requests[i].Events {
			var e nostr.Event
			if e.UnmarshalJSON([]byte(req.Event.Payload)) == nil && e.Kind == kind {
				return e
			}
		}
	}
	t.Fatalf("kind %d missing", kind)
	return nostr.Event{}
}
func (s *recordingStore) assertEventScope(t *testing.T, kind nostr.Kind, scope store.SourceScope, uri string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, reconciliation := range s.requests {
		for _, request := range reconciliation.Events {
			var event nostr.Event
			_ = event.UnmarshalJSON([]byte(request.Event.Payload))
			if event.Kind == kind && request.Mapping.Source.Scope == scope && request.Mapping.Source.URI == uri {
				return
			}
		}
	}
	t.Fatalf("kind %d mapping %v/%s missing", kind, scope, uri)
}
func identity(provider, id string) source.ActorIdentity {
	return source.ActorIdentity{Provider: provider, ID: id}
}
func snapshot(ids ...source.ActorIdentity) source.TargetSnapshot {
	set := source.IdentitySet{}
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return source.TargetSnapshot{Follows: set, Union: set, Lists: map[string]source.List{}}
}
func assertFollows(t *testing.T, event nostr.Event, ids []source.ActorIdentity) {
	t.Helper()
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		key, _ := nostrmap.DeriveActorKey([]byte("01234567890123456789012345678901"), id)
		want[key.Public().Hex()] = struct{}{}
		if event.Tags.FindWithValue("p", key.Public().Hex()) == nil {
			t.Errorf("missing %#v", id)
		}
	}
	got := 0
	for _, tag := range event.Tags {
		if len(tag) > 1 && tag[0] == "p" {
			got++
			if _, ok := want[tag[1]]; !ok {
				t.Errorf("unexpected p tag %s", tag[1])
			}
		}
	}
	if got != len(want) {
		t.Errorf("p tag count = %d, want %d", got, len(want))
	}
}
