package syncer

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/bluesky"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/nostrmap"
	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

func TestBackfillAndReplayEnqueueOneEventForOneSourceURI(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	s := New(Options{Source: fakeSource{pages: []bluesky.Page{{Posts: []bluesky.Post{{URI: "at://did:plc:alice/app.bsky.feed.post/one", AuthorDID: "did:plc:alice", Text: "hello", CreatedAt: time.Unix(10, 0)}}}}}, OutboxStore: store, MasterSeed: []byte("seed"), BackfillLimit: 100})

	if err := s.Backfill(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Handle(ctx, Event{DID: "did:plc:alice", Collection: postCollection, RKey: "one", Operation: Create, TimeUS: 20_000_000, Record: json.RawMessage(`{"text":"hello","createdAt":"1970-01-01T00:00:10Z"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EventMappingBySourceURI(ctx, "at://did:plc:alice/app.bsky.feed.post/one"); err != nil {
		t.Fatal(err)
	}
}

func TestBackfillFiltersTargetsWithoutConsumingLimitAndContinuesPages(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	source := &recordingSource{pages: map[string]bluesky.Page{
		"": {Posts: []bluesky.Post{
			{URI: "at://did:plc:outside/app.bsky.feed.post/one", AuthorDID: "did:plc:outside", Text: "outside"},
			{URI: "at://did:plc:alice/app.bsky.feed.post/two", AuthorDID: "did:plc:alice", Text: "inside"},
		}, Cursor: "next"},
		"next": {Posts: []bluesky.Post{{URI: "at://did:plc:bob/app.bsky.feed.post/three", AuthorDID: "did:plc:bob", Text: "inside list"}}},
	}}
	s := New(Options{Source: source, OutboxStore: store, MasterSeed: []byte("seed"),
		Targets: bluesky.DIDSet{"did:plc:alice": {}, "did:plc:bob": {}}, BackfillLimit: 2})

	if err := s.Backfill(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 2 {
		t.Fatalf("mapped events = %d, want two target posts", len(store.events))
	}
	if _, ok := store.events["at://did:plc:outside/app.bsky.feed.post/one"]; ok {
		t.Fatal("non-target post was mapped")
	}
	if got := source.calls; len(got) != 2 || got[0].cursor != "" || got[0].limit != 2 || got[1].cursor != "next" || got[1].limit != 1 {
		t.Fatalf("timeline calls = %#v", got)
	}
}

func TestEmptyTargetProviderDeniesBackfillAndLive(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	source := &recordingSource{pages: map[string]bluesky.Page{"": {Posts: []bluesky.Post{{URI: "at://did:plc:alice/app.bsky.feed.post/one", AuthorDID: "did:plc:alice"}}}}}
	s := New(Options{Source: source, OutboxStore: store, MasterSeed: []byte("seed"), TargetProvider: func() bluesky.DIDSet { return bluesky.DIDSet{} }})
	if err := s.Backfill(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Handle(ctx, Event{DID: "did:plc:alice", Collection: postCollection, RKey: "live", Operation: Create, Record: json.RawMessage(`{"text":"live","createdAt":"1970-01-01T00:00:10Z"}`)}); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 0 {
		t.Fatalf("mapped events = %d, want none", len(store.events))
	}
}

func TestBackfillEnqueuesAllowThenEventWithoutDirectRelay(t *testing.T) {
	ctx := context.Background()
	durable, closer, err := bridgestore.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	s := New(Options{Source: fakeSource{pages: []bluesky.Page{{Posts: []bluesky.Post{{URI: "at://did:plc:alice/app.bsky.feed.post/one", AuthorDID: "did:plc:alice", Text: "hello", CreatedAt: time.Unix(10, 0)}}}}}, OutboxStore: durable, MasterSeed: []byte("seed"), BackfillLimit: 100, OutboxLimit: 10})
	if err := s.Backfill(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := durable.ClaimOutbox(ctx, time.Now().Add(time.Second), time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Operation != bridgestore.OutboxAllowPublisher {
		t.Fatalf("first claim = %#v", items)
	}
	if err := durable.CompletePublisherRegistration(ctx, items[0].ID, items[0].ClaimToken, items[0].PubKey, time.Now()); err != nil {
		t.Fatal(err)
	}
	items, err = durable.ClaimOutbox(ctx, time.Now().Add(time.Second), time.Minute, 10)
	if err != nil || len(items) != 1 || items[0].Operation != bridgestore.OutboxPublishEvent {
		t.Fatalf("second claim = %#v, %v", items, err)
	}
}

func TestBackfillPublishesTimelineImages(t *testing.T) {
	ctx := context.Background()
	durable, closer, err := bridgestore.Open(ctx, filepath.Join(t.TempDir(), "backfill-images.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	post := bluesky.Post{URI: "at://did:plc:alice/app.bsky.feed.post/image", AuthorDID: "did:plc:alice", Text: "photo", CreatedAt: time.Unix(10, 0), Images: []bluesky.Image{{URL: "https://cdn.bsky.app/image.jpg", MIMEType: "image/jpeg", Alt: "Photo"}}}
	s := New(Options{Source: fakeSource{pages: []bluesky.Page{{Posts: []bluesky.Post{post}}}}, OutboxStore: durable, MasterSeed: []byte("seed"), OutboxLimit: 10})
	if err := s.Backfill(ctx); err != nil {
		t.Fatal(err)
	}
	event := claimPublishedEvent(t, ctx, durable)
	if !strings.Contains(event.Content, "https://cdn.bsky.app/image.jpg") || event.Tags.Find("imeta") == nil {
		t.Fatalf("image event = content %q, tags %#v", event.Content, event.Tags)
	}
}

func TestJetstreamPublishesImageBlobAsBlueskyCDNURL(t *testing.T) {
	ctx := context.Background()
	durable, closer, err := bridgestore.Open(ctx, filepath.Join(t.TempDir(), "jetstream-images.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	s := New(Options{OutboxStore: durable, MasterSeed: []byte("seed"), OutboxLimit: 10})
	record := json.RawMessage(`{"text":"photo","createdAt":"1970-01-01T00:00:10Z","embed":{"$type":"app.bsky.embed.images","images":[{"alt":"Photo","image":{"$type":"blob","ref":{"$link":"bafkreihash"},"mimeType":"image/jpeg","size":123},"aspectRatio":{"width":640,"height":480}}]}}`)
	if err := s.Handle(ctx, Event{DID: "did:plc:alice", Collection: postCollection, RKey: "image", Operation: Create, Record: record}); err != nil {
		t.Fatal(err)
	}
	event := claimPublishedEvent(t, ctx, durable)
	want := "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:alice/bafkreihash@jpeg"
	if !strings.Contains(event.Content, want) || event.Tags.Find("imeta") == nil {
		t.Fatalf("image event = content %q, tags %#v", event.Content, event.Tags)
	}
}

func claimPublishedEvent(t *testing.T, ctx context.Context, store bridgestore.SQLiteStore) nostr.Event {
	t.Helper()
	items, err := store.ClaimOutbox(ctx, time.Now().Add(time.Second), time.Minute, 10)
	if err != nil || len(items) == 0 {
		t.Fatalf("claim publisher registration = %#v, %v", items, err)
	}
	if items[0].Operation == bridgestore.OutboxAllowPublisher {
		if err := store.CompletePublisherRegistration(ctx, items[0].ID, items[0].ClaimToken, items[0].PubKey, time.Now()); err != nil {
			t.Fatal(err)
		}
		items, err = store.ClaimOutbox(ctx, time.Now().Add(time.Second), time.Minute, 10)
	}
	if err != nil || len(items) == 0 {
		t.Fatalf("claim publish = %#v, %v", items, err)
	}
	var event nostr.Event
	if err := json.Unmarshal([]byte(items[0].Payload), &event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestRegisteredPublisherSkipsAllowEnqueue(t *testing.T) {
	ctx := context.Background()
	durable, closer, err := bridgestore.Open(ctx, filepath.Join(t.TempDir(), "registered.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	key, _ := nostrmap.DeriveKey([]byte("seed"), "did:plc:alice")
	_ = durable.SetPublisherRegistered(ctx, key.Public().Hex(), time.Now())
	s := New(Options{Source: fakeSource{pages: []bluesky.Page{{Posts: []bluesky.Post{{URI: "at://did:plc:alice/app.bsky.feed.post/one", AuthorDID: "did:plc:alice", Text: "hello", CreatedAt: time.Unix(10, 0)}}}}}, OutboxStore: durable, MasterSeed: []byte("seed"), OutboxLimit: 1})
	if err := s.Backfill(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := durable.ClaimOutbox(ctx, time.Now().Add(time.Second), time.Minute, 1)
	if err != nil || len(items) != 1 || items[0].Operation != bridgestore.OutboxPublishEvent {
		t.Fatalf("items = %#v, %v", items, err)
	}
}

func TestOutboxOnlyCreateAndDeletePersistCursorAndKindFive(t *testing.T) {
	ctx := context.Background()
	durable, closer, err := bridgestore.Open(ctx, filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	s := New(Options{OutboxStore: durable, MasterSeed: []byte("seed"), OutboxLimit: 10})
	created := Event{DID: "did:plc:alice", Collection: postCollection, RKey: "one", Operation: Create, TimeUS: 10, Record: json.RawMessage(`{"text":"hello","createdAt":"1970-01-01T00:00:10Z"}`)}
	if err := s.Handle(ctx, created); err != nil {
		t.Fatal(err)
	}
	if cursor, err := durable.Cursor(ctx, jetstreamCursor); err != nil || cursor != 10 {
		t.Fatalf("cursor = %d, %v", cursor, err)
	}
	items, _ := durable.ClaimOutbox(ctx, time.Now().Add(time.Second), time.Minute, 1)
	_ = durable.CompletePublisherRegistration(ctx, items[0].ID, items[0].ClaimToken, items[0].PubKey, time.Now())
	items, _ = durable.ClaimOutbox(ctx, time.Now().Add(time.Second), time.Minute, 1)
	_ = durable.CompleteOutbox(ctx, items[0].ID, items[0].ClaimToken, time.Now())
	if err := s.Handle(ctx, Event{DID: created.DID, Collection: created.Collection, RKey: created.RKey, Operation: Delete, TimeUS: 20}); err != nil {
		t.Fatal(err)
	}
	items, err = durable.ClaimOutbox(ctx, time.Now().Add(time.Second), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("delete claim = %#v, %v", items, err)
	}
	var deletion nostr.Event
	if err := json.Unmarshal([]byte(items[0].Payload), &deletion); err != nil || deletion.Kind != 5 {
		t.Fatalf("deletion = %#v, %v", deletion, err)
	}
	if cursor, err := durable.Cursor(ctx, jetstreamCursor); err != nil || cursor != 20 {
		t.Fatalf("delete cursor = %d, %v", cursor, err)
	}
}

func TestReplyUsesPersistedParentMappingForEAndPTags(t *testing.T) {
	ctx := context.Background()
	durable, closer, err := bridgestore.Open(ctx, filepath.Join(t.TempDir(), "reply.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	parentKey, _ := nostrmap.DeriveKey([]byte("seed"), "did:plc:parent")
	parent := nostr.Event{CreatedAt: nostr.Now(), Kind: 1}
	if err := parent.Sign(parentKey); err != nil {
		t.Fatal(err)
	}
	if err := durable.SaveEventMapping(ctx, bridgestore.EventMapping{SourceURI: "at://did:plc:parent/app.bsky.feed.post/one", NostrEventID: parent.ID.Hex(), AuthorPubKey: parent.PubKey.Hex()}); err != nil {
		t.Fatal(err)
	}
	s := New(Options{OutboxStore: durable, MasterSeed: []byte("seed"), OutboxLimit: 10})
	event := Event{DID: "did:plc:alice", Collection: postCollection, RKey: "reply", Operation: Create, Record: json.RawMessage(`{"text":"reply","createdAt":"1970-01-01T00:00:10Z","reply":{"parent":{"uri":"at://did:plc:parent/app.bsky.feed.post/one"}}}`)}
	if err := s.Handle(ctx, event); err != nil {
		t.Fatal(err)
	}
	items, err := durable.ClaimOutbox(ctx, time.Now().Add(time.Second), time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("no outbox items")
	}
	if items[0].Operation == bridgestore.OutboxAllowPublisher {
		if err := durable.CompletePublisherRegistration(ctx, items[0].ID, items[0].ClaimToken, items[0].PubKey, time.Now()); err != nil {
			t.Fatal(err)
		}
		items, err = durable.ClaimOutbox(ctx, time.Now().Add(time.Second), time.Minute, 10)
		if err != nil {
			t.Fatal(err)
		}
	}
	var reply nostr.Event
	if err := json.Unmarshal([]byte(items[len(items)-1].Payload), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Tags.FindWithValue("e", parent.ID.Hex()) == nil || reply.Tags.FindWithValue("p", parent.PubKey.Hex()) == nil {
		t.Fatalf("reply tags = %#v", reply.Tags)
	}
}

func TestReconnectCursorRewindsAndSubscriptionFiltersTargetsAndCollections(t *testing.T) {
	store := newMemoryStore()
	if err := store.SaveCursor(context.Background(), jetstreamCursor, 12_000_000); err != nil {
		t.Fatal(err)
	}
	s := New(Options{OutboxStore: store, Targets: bluesky.DIDSet{"did:plc:alice": {}}, Rewind: 3 * time.Second})
	request, err := s.Subscription(context.Background(), "wss://jetstream.example/subscribe")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := request.Query().Get("cursor"), "9000000"; got != want {
		t.Fatalf("cursor = %q, want %q", got, want)
	}
	if got, want := request.Query()["wantedDids"], []string{"did:plc:alice"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("wantedDids = %#v", got)
	}
	if got, want := request.Query()["wantedCollections"], []string{postCollection, repostCollection}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("wantedCollections = %#v", got)
	}
}

func TestSubscriptionUsesOutboxStoreWithoutLegacyStore(t *testing.T) {
	durable, closer, err := bridgestore.Open(context.Background(), filepath.Join(t.TempDir(), "subscription.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	_ = durable.SaveCursor(context.Background(), jetstreamCursor, 12_000_000)
	s := New(Options{OutboxStore: durable, Targets: bluesky.DIDSet{"did:plc:alice": {}}, Rewind: 3 * time.Second})
	u, err := s.Subscription(context.Background(), "wss://jetstream.example/subscribe")
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("cursor"); got != "9000000" {
		t.Fatalf("cursor = %q", got)
	}
}

func TestRunReportsJetstreamConnectionAndEventToObserver(t *testing.T) {
	store := newMemoryStore()
	observer := &recordingObserver{}
	ctx, cancel := context.WithCancel(context.Background())
	connection := &fakeConnection{messages: [][]byte{[]byte(`{"did":"did:plc:alice","time_us":10,"commit":{"operation":"create","collection":"app.bsky.feed.post","rkey":"one","record":{"text":"hello","createdAt":"1970-01-01T00:00:10Z"}}}`)}, cancel: cancel}
	s := New(Options{OutboxStore: store, MasterSeed: []byte("seed"), Targets: bluesky.DIDSet{"did:plc:alice": {}}, JetstreamURL: "ws://jetstream.example", Source: fakeSource{pages: []bluesky.Page{{}}}, Connect: func(context.Context, string) (Connection, error) { return connection, nil }, Observer: observer})
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if observer.connected || observer.events != 1 || observer.lastSync.IsZero() {
		t.Fatalf("observer = %#v, want disconnected after one reported event and sync", observer)
	}
}

func TestRunReconnectsJetstreamWhenTargetFilterChanges(t *testing.T) {
	store := newMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mutex sync.Mutex
	targets := bluesky.DIDSet{"did:plc:alice": {}}
	targetUpdates := make(chan struct{}, 1)
	connected := make(chan struct{})
	urls := make([]string, 0, 2)
	connections := 0
	s := New(Options{
		OutboxStore:  store,
		MasterSeed:   []byte("seed"),
		Source:       fakeSource{pages: []bluesky.Page{{}}},
		JetstreamURL: "ws://jetstream.example",
		TargetProvider: func() bluesky.DIDSet {
			mutex.Lock()
			defer mutex.Unlock()
			return targets
		},
		TargetUpdates: targetUpdates,
		Connect: func(_ context.Context, endpoint string) (Connection, error) {
			mutex.Lock()
			defer mutex.Unlock()
			urls = append(urls, endpoint)
			connections++
			if connections == 1 {
				close(connected)
				return blockingConnection{}, nil
			}
			return cancelingConnection{cancel: cancel}, nil
		},
	})
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-connected:
		mutex.Lock()
		targets = bluesky.DIDSet{"did:plc:bob": {}}
		mutex.Unlock()
		targetUpdates <- struct{}{}
	case <-time.After(time.Second):
		t.Fatal("first Jetstream connection was not established")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("syncer did not reconnect after the target filter changed")
	}
	if len(urls) != 2 {
		t.Fatalf("connections = %d, want 2", len(urls))
	}
	first, err := url.Parse(urls[0])
	if err != nil {
		t.Fatal(err)
	}
	second, err := url.Parse(urls[1])
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Query()["wantedDids"]; len(got) != 1 || got[0] != "did:plc:alice" {
		t.Fatalf("first filter = %#v", got)
	}
	if got := second.Query()["wantedDids"]; len(got) != 1 || got[0] != "did:plc:bob" {
		t.Fatalf("second filter = %#v", got)
	}
}

type fakeSource struct{ pages []bluesky.Page }

type timelineCall struct {
	cursor string
	limit  int
}

type recordingSource struct {
	pages map[string]bluesky.Page
	calls []timelineCall
}

func (f *recordingSource) Timeline(_ context.Context, cursor string, limit int) (bluesky.Page, error) {
	f.calls = append(f.calls, timelineCall{cursor: cursor, limit: limit})
	return f.pages[cursor], nil
}
func (*recordingSource) Follows(context.Context) ([]bluesky.Actor, error) { return nil, nil }
func (*recordingSource) List(context.Context, string) (bluesky.List, error) {
	return bluesky.List{}, nil
}
func (*recordingSource) Profile(context.Context, string) (bluesky.Profile, error) {
	return bluesky.Profile{}, nil
}

func (f fakeSource) Timeline(_ context.Context, cursor string, _ int) (bluesky.Page, error) {
	if cursor == "" {
		return f.pages[0], nil
	}
	return bluesky.Page{}, nil
}
func (fakeSource) Follows(context.Context) ([]bluesky.Actor, error)   { return nil, nil }
func (fakeSource) List(context.Context, string) (bluesky.List, error) { return bluesky.List{}, nil }
func (fakeSource) Profile(context.Context, string) (bluesky.Profile, error) {
	return bluesky.Profile{}, nil
}

type memoryStore struct {
	events     map[string]bridgestore.EventMapping
	operations map[string]string
	cursors    map[string]int64
}

func newMemoryStore() *memoryStore {
	return &memoryStore{events: map[string]bridgestore.EventMapping{}, operations: map[string]string{}, cursors: map[string]int64{}}
}
func (m *memoryStore) EventMappingBySourceURI(_ context.Context, uri string) (bridgestore.EventMapping, error) {
	e, ok := m.events[uri]
	if !ok {
		return bridgestore.EventMapping{}, sql.ErrNoRows
	}
	return e, nil
}
func (m *memoryStore) SourceOperationBySourceURI(_ context.Context, uri string) (string, error) {
	identity, ok := m.operations[uri]
	if !ok {
		return "", sql.ErrNoRows
	}
	return identity, nil
}
func (m *memoryStore) SaveCursor(_ context.Context, name string, value int64) error {
	m.cursors[name] = value
	return nil
}
func (m *memoryStore) Cursor(_ context.Context, name string) (int64, error) {
	return m.cursors[name], nil
}
func (m *memoryStore) EnqueueEvent(_ context.Context, request bridgestore.EventEnqueueRequest) error {
	m.events[request.Mapping.SourceURI] = request.Mapping
	if request.SourceOperation != "" {
		m.operations[request.Mapping.SourceURI] = request.SourceOperation
	}
	if request.Cursor != nil {
		m.cursors[request.Cursor.Name] = request.Cursor.Value
	}
	return nil
}
func (m *memoryStore) EnqueueDelete(_ context.Context, request bridgestore.DeleteEnqueueRequest) error {
	delete(m.events, request.SourceURI)
	if request.Cursor != nil {
		m.cursors[request.Cursor.Name] = request.Cursor.Value
	}
	return nil
}
func (m *memoryStore) EnqueueUpdate(_ context.Context, request bridgestore.UpdateEnqueueRequest) error {
	m.events[request.Mapping.SourceURI] = request.Mapping
	m.operations[request.Mapping.SourceURI] = request.SourceOperation
	if request.Cursor != nil {
		m.cursors[request.Cursor.Name] = request.Cursor.Value
	}
	return nil
}

type fakeConnection struct {
	messages [][]byte
	cancel   context.CancelFunc
}

func (c *fakeConnection) Read(context.Context) ([]byte, error) {
	if len(c.messages) == 0 {
		return nil, context.Canceled
	}
	message := c.messages[0]
	c.messages = c.messages[1:]
	c.cancel()
	return message, nil
}
func (*fakeConnection) Close() error { return nil }

type blockingConnection struct{}

func (blockingConnection) Read(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingConnection) Close() error { return nil }

type cancelingConnection struct{ cancel context.CancelFunc }

func (c cancelingConnection) Read(context.Context) ([]byte, error) {
	c.cancel()
	return nil, context.Canceled
}
func (cancelingConnection) Close() error { return nil }

type recordingObserver struct {
	connected bool
	events    int
	lastSync  time.Time
	pending   int
}

func (o *recordingObserver) JetstreamConnected(connected bool) { o.connected = connected }
func (o *recordingObserver) JetstreamEvent(time.Time)          { o.events++ }
func (o *recordingObserver) SyncCompleted(at time.Time)        { o.lastSync = at }
func (o *recordingObserver) PendingWork(delta int)             { o.pending += delta }
