package mastodon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

const testSeed = "01234567890123456789012345678901"

type fakeTimelineAPI struct {
	home     []Status
	lists    map[string][]Status
	calls    []string
	pageSize int
}

func (f *fakeTimelineAPI) HomeTimeline(_ context.Context, since, max string, limit int) (TimelinePage, error) {
	f.calls = append(f.calls, "home")
	return f.page(f.home, since, max, limit), nil
}
func (f *fakeTimelineAPI) ListTimeline(_ context.Context, id, since, max string, limit int) (TimelinePage, error) {
	f.calls = append(f.calls, "list:"+id)
	return f.page(f.lists[id], since, max, limit), nil
}
func (f *fakeTimelineAPI) page(all []Status, since, max string, limit int) TimelinePage {
	var v []Status
	for _, s := range all {
		if since != "" && s.ID <= since {
			continue
		}
		if max != "" && s.ID >= max {
			continue
		}
		v = append(v, s)
	}
	n := limit
	if f.pageSize > 0 && f.pageSize < n {
		n = f.pageSize
	}
	if n > len(v) {
		n = len(v)
	}
	p := TimelinePage{Statuses: append([]Status(nil), v[:n]...)}
	if n < len(v) && n > 0 {
		p.NextMaxID = v[n-1].ID
	}
	return p
}

type memoryDelivery struct {
	mappings   map[string]bridgestore.EventMapping
	operations map[string]string
	cursors    map[string]string
	payloads   []nostr.Event
	failCursor string
}

func newMemoryDelivery() *memoryDelivery {
	return &memoryDelivery{mappings: map[string]bridgestore.EventMapping{}, operations: map[string]string{}, cursors: map[string]string{}}
}
func (m *memoryDelivery) key(r bridgestore.SourceRef) string {
	return r.Scope.Provider + "\x00" + r.Scope.Account + "\x00" + r.URI
}
func (m *memoryDelivery) add(payload string) {
	var e nostr.Event
	if json.Unmarshal([]byte(payload), &e) == nil {
		m.payloads = append(m.payloads, e)
	}
}
func (m *memoryDelivery) EnqueueEvent(_ context.Context, r bridgestore.EventEnqueueRequest) error {
	k := m.key(r.Mapping.Source)
	if _, ok := m.mappings[k]; ok {
		return nil
	}
	m.mappings[k] = r.Mapping
	m.operations[k] = r.SourceOperation
	m.add(r.Event.Payload)
	if r.Cursor != nil {
		m.cursors[r.Cursor.Name] = r.Cursor.Value
	}
	return nil
}
func (m *memoryDelivery) EnqueueDelete(_ context.Context, r bridgestore.DeleteEnqueueRequest) error {
	k := m.key(r.Source)
	if _, ok := m.mappings[k]; !ok {
		return sql.ErrNoRows
	}
	delete(m.mappings, k)
	m.add(r.Event.Payload)
	if r.Cursor != nil {
		m.cursors[r.Cursor.Name] = r.Cursor.Value
	}
	return nil
}
func (m *memoryDelivery) EnqueueUpdate(_ context.Context, r bridgestore.UpdateEnqueueRequest) error {
	k := m.key(r.Mapping.Source)
	m.mappings[k] = r.Mapping
	m.operations[k] = r.SourceOperation
	m.add(r.Deletion.Payload)
	m.add(r.Replacement.Payload)
	if r.Cursor != nil {
		m.cursors[r.Cursor.Name] = r.Cursor.Value
	}
	return nil
}
func (m *memoryDelivery) EventMappingBySourceURI(_ context.Context, r bridgestore.SourceRef) (bridgestore.EventMapping, error) {
	v, ok := m.mappings[m.key(r)]
	if !ok {
		return v, sql.ErrNoRows
	}
	return v, nil
}
func (m *memoryDelivery) SourceOperationBySourceURI(_ context.Context, r bridgestore.SourceRef) (string, error) {
	v, ok := m.operations[m.key(r)]
	if !ok {
		return "", sql.ErrNoRows
	}
	return v, nil
}
func (m *memoryDelivery) SaveCursor(_ context.Context, _ bridgestore.SourceScope, n, v string) error {
	if n == m.failCursor {
		return errors.New("cursor failure")
	}
	m.cursors[n] = v
	return nil
}
func (m *memoryDelivery) Cursor(_ context.Context, _ bridgestore.SourceScope, n string) (string, error) {
	v, ok := m.cursors[n]
	if !ok {
		return "", sql.ErrNoRows
	}
	return v, nil
}

func testStatus(id, uri, actor string) Status {
	return Status{ID: id, URI: uri, URL: uri, Content: "<p>hello</p>", Visibility: "public", CreatedAt: time.Unix(10, 0), Account: Account{URI: actor}}
}
func testSyncer(api TimelineAPI, store *memoryDelivery, targets source.IdentitySet) *Syncer {
	return NewSyncer(SyncOptions{Scope: bridgestore.SourceScope{Provider: "mastodon", Account: "owner@example"}, API: api, Store: store, MasterSeed: []byte(testSeed), Targets: func() source.IdentitySet { return targets }, ListIDs: []string{"7"}, BackfillLimit: 20})
}

func TestBackfillDeduplicatesHomeAndListByURI(t *testing.T) {
	post := testStatus("42", "https://social.example/users/alice/statuses/42", "https://social.example/users/alice")
	api := &fakeTimelineAPI{home: []Status{post}, lists: map[string][]Status{"7": {post}}}
	store := newMemoryDelivery()
	s := testSyncer(api, store, source.IdentitySet{{Provider: "mastodon", ID: post.Account.URI}: {}})
	if err := s.Backfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.payloads) != 1 {
		t.Fatalf("published=%d", len(store.payloads))
	}
}

func TestBackfillFiltersNonTargetsAndPersistsReplyAlias(t *testing.T) {
	parent := testStatus("41", "https://social.example/users/alice/statuses/41", "https://social.example/users/alice")
	child := testStatus("42", "https://social.example/users/alice/statuses/42", parent.Account.URI)
	child.InReplyToID = "41"
	other := testStatus("50", "https://social.example/users/mallory/statuses/50", "https://social.example/users/mallory")
	store := newMemoryDelivery()
	s := testSyncer(&fakeTimelineAPI{home: []Status{parent, child, other}, lists: map[string][]Status{}}, store, source.IdentitySet{{Provider: "mastodon", ID: parent.Account.URI}: {}})
	if err := s.Backfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.payloads) != 2 {
		t.Fatalf("published=%d", len(store.payloads))
	}
	if !hasTag(store.payloads[1], "e") {
		t.Fatal("reply tag missing")
	}
	if got := store.cursors[aliasCursor("41")]; got != "mastodon:"+parent.URI {
		t.Fatalf("alias=%q", got)
	}
}

func TestHandleEventUpdateDeleteAndDuplicateReplay(t *testing.T) {
	status := testStatus("42", "https://social.example/users/alice/statuses/42", "https://social.example/users/alice")
	store := newMemoryDelivery()
	s := testSyncer(nil, store, source.IdentitySet{{Provider: "mastodon", ID: status.Account.URI}: {}})
	ctx := context.Background()
	for _, event := range []StreamEvent{{Event: "update", Payload: status}, {Event: "update", Payload: status}} {
		if err := s.HandleEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	status.Content = "edited"
	if err := s.HandleEvent(ctx, StreamEvent{Event: "status.update", Payload: status}); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleEvent(ctx, StreamEvent{Event: "delete", DeleteID: "42"}); err != nil {
		t.Fatal(err)
	}
	if len(store.payloads) != 4 {
		t.Fatalf("events=%d", len(store.payloads))
	}
	if store.payloads[1].Kind != 5 || store.payloads[3].Kind != 5 {
		t.Fatalf("kinds=%d,%d", store.payloads[1].Kind, store.payloads[3].Kind)
	}
}

func TestDeleteAndReplyAliasesSurviveSyncerRestart(t *testing.T) {
	parent := testStatus("41", "https://social.example/users/alice/statuses/41", "https://social.example/users/alice")
	store := newMemoryDelivery()
	targets := source.IdentitySet{{Provider: "mastodon", ID: parent.Account.URI}: {}}
	if err := testSyncer(nil, store, targets).HandleEvent(context.Background(), StreamEvent{Event: "update", Payload: parent}); err != nil {
		t.Fatal(err)
	}
	child := testStatus("42", "https://social.example/users/alice/statuses/42", parent.Account.URI)
	child.InReplyToID = "41"
	restarted := testSyncer(nil, store, targets)
	if err := restarted.HandleEvent(context.Background(), StreamEvent{Event: "update", Payload: child}); err != nil {
		t.Fatal(err)
	}
	if !hasTag(store.payloads[1], "e") {
		t.Fatal("restart lost reply alias")
	}
	if err := restarted.HandleEvent(context.Background(), StreamEvent{Event: "delete", DeleteID: "41"}); err != nil {
		t.Fatal(err)
	}
}

func hasTag(event nostr.Event, name string) bool {
	for _, tag := range event.Tags {
		if len(tag) > 0 && tag[0] == name {
			return true
		}
	}
	return false
}

func TestBackfillWindowDrainsMoreThanLimitAcrossRestartExactlyOnce(t *testing.T) {
	actor := "https://social.example/users/alice"
	var statuses []Status
	for i := 9; i >= 1; i-- {
		statuses = append(statuses, testStatus(fmt.Sprint(i), fmt.Sprintf("%s/statuses/%d", actor, i), actor))
	}
	api := &fakeTimelineAPI{home: statuses, lists: map[string][]Status{}, pageSize: 2}
	store := newMemoryDelivery()
	targets := source.IdentitySet{{Provider: "mastodon", ID: actor}: {}}
	for i := 0; i < 6; i++ {
		s := NewSyncer(SyncOptions{Scope: bridgestore.SourceScope{Provider: "mastodon", Account: "owner"}, API: api, Store: store, MasterSeed: []byte(testSeed), Targets: func() source.IdentitySet { return targets }, BackfillLimit: 3})
		if err := s.Backfill(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.payloads) != 9 {
		t.Fatalf("published=%d", len(store.payloads))
	}
	seen := map[string]bool{}
	for _, e := range store.payloads {
		key := e.ID.Hex()
		if seen[key] {
			t.Fatal("duplicate publication")
		}
		seen[key] = true
	}
}

func TestBackfillDetectsDisconnectedEditAfterRestart(t *testing.T) {
	status := testStatus("42", "https://social.example/users/alice/statuses/42", "https://social.example/users/alice")
	api := &fakeTimelineAPI{home: []Status{status}, lists: map[string][]Status{}}
	store := newMemoryDelivery()
	targets := source.IdentitySet{{Provider: "mastodon", ID: status.Account.URI}: {}}
	s := NewSyncer(SyncOptions{Scope: bridgestore.SourceScope{Provider: "mastodon", Account: "owner"}, API: api, Store: store, MasterSeed: []byte(testSeed), Targets: func() source.IdentitySet { return targets }})
	if err := s.Backfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	status.Content = "<p>edited offline</p>"
	api.home = []Status{status}
	store.cursors[feedCommitted("home")] = ""
	if err := s.Backfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.payloads) != 3 || store.payloads[1].Kind != 5 {
		t.Fatalf("events=%d", len(store.payloads))
	}
}

func TestBackfillFilteredPagesAdvanceToOlderTarget(t *testing.T) {
	target := "https://social.example/users/alice"
	other := "https://social.example/users/other"
	statuses := []Status{testStatus("5", other+"/statuses/5", other), testStatus("4", other+"/statuses/4", other), testStatus("3", other+"/statuses/3", other), testStatus("2", target+"/statuses/2", target)}
	api := &fakeTimelineAPI{home: statuses, lists: map[string][]Status{}, pageSize: 2}
	store := newMemoryDelivery()
	s := NewSyncer(SyncOptions{Scope: bridgestore.SourceScope{Provider: "mastodon", Account: "owner"}, API: api, Store: store, MasterSeed: []byte(testSeed), Targets: func() source.IdentitySet { return source.IdentitySet{{Provider: "mastodon", ID: target}: {}} }, BackfillLimit: 2})
	if err := s.Backfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Backfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.payloads) != 1 {
		t.Fatalf("published=%d", len(store.payloads))
	}
}

func TestBackfillDoesNotAdvanceCommittedBoundaryWhenCursorStoreFails(t *testing.T) {
	status := testStatus("42", "https://social.example/users/alice/statuses/42", "https://social.example/users/alice")
	api := &fakeTimelineAPI{home: []Status{status}, lists: map[string][]Status{}}
	store := newMemoryDelivery()
	store.failCursor = feedCommitted("home")
	s := NewSyncer(SyncOptions{Scope: bridgestore.SourceScope{Provider: "mastodon", Account: "owner"}, API: api, Store: store, MasterSeed: []byte(testSeed), Targets: func() source.IdentitySet {
		return source.IdentitySet{{Provider: "mastodon", ID: status.Account.URI}: {}}
	}})
	if err := s.Backfill(context.Background()); err == nil {
		t.Fatal("expected cursor failure")
	}
	if store.cursors[feedCommitted("home")] != "" {
		t.Fatal("unsafe committed boundary")
	}
}
