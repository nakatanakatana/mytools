package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

var testPubKey2 = nostr.MustPubKeyFromHex("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")

func TestSQLiteStoreUsesGeneratedQueries(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate sqlite_test.go")
	}
	dir := filepath.Dir(filename)
	rawSQL := regexp.MustCompile(`(?i)[\x60\x22][^\x60\x22]*(SELECT|INSERT|UPDATE|DELETE)\b`)
	for _, name := range []string{"sqlite.go", "query_events.go"} {
		source, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"ExecContext", "QueryContext", "QueryRowContext"} {
			if regexp.MustCompile(`\b` + forbidden + `\b`).Match(source) {
				t.Errorf("%s contains forbidden database call %s", name, forbidden)
			}
		}
		if match := rawSQL.Find(source); match != nil {
			t.Errorf("%s contains raw SQL string %q", name, match)
		}
	}
}

func TestSQLiteStoreEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t, filepath.Join(t.TempDir(), "relay.sqlite"))

	e1 := testEvent(1, nostr.NUMS, 10, 1, nil, "one")
	e2 := testEvent(2, testPubKey2, 20, 2, nostr.Tags{{"p", nostr.NUMS.Hex()}}, "two")
	if err := s.SaveEvent(ctx, e1); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEvent(ctx, e2); err != nil {
		t.Fatal(err)
	}

	got, err := s.Event(ctx, e1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, e1) {
		t.Fatalf("Event() = %#v, want %#v", got, e1)
	}

	seq, err := s.QueryEvents(ctx, nostr.Filter{
		Authors: []nostr.PubKey{testPubKey2}, Kinds: []nostr.Kind{2},
		Tags: nostr.TagMap{"p": {nostr.NUMS.Hex()}}, Since: 15, Until: 25, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if events := slices.Collect(seq); !slices.EqualFunc(events, []nostr.Event{e2}, eventsEqual) {
		t.Fatalf("QueryEvents() = %#v, want %#v", events, []nostr.Event{e2})
	}

	if err := s.DeleteEvent(ctx, e1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Event(ctx, e1.ID); err == nil {
		t.Fatal("Event() after DeleteEvent() returned nil error")
	}
}

func TestSQLiteStoreKindFiveRollbackIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, filepath.Join(t.TempDir(), "relay.sqlite"))
	owner := testPubKey2
	target := testEvent(10, owner, 10, 1, nil, "target")
	deletion := testEvent(11, owner, 20, 5, nostr.Tags{{"e", target.ID.Hex()}}, "delete")
	if err := s.SaveEvent(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`CREATE TRIGGER fail_event_delete BEFORE DELETE ON events BEGIN SELECT RAISE(FAIL, 'injected delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEventAndApplyDeletion(ctx, deletion); err == nil {
		t.Fatal("SaveEventAndApplyDeletion() returned nil error")
	}
	if _, err := s.Event(ctx, target.ID); err != nil {
		t.Fatalf("target was not rolled back: %v", err)
	}
	if _, err := s.Event(ctx, deletion.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deletion event persisted after rollback: %v", err)
	}
}

func TestSQLiteStoreDeletionTombstoneRejectsReplayAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite")
	owner := testPubKey2
	target := testEvent(20, owner, 10, 1, nil, "target")
	deletion := testEvent(21, owner, 20, 5, nostr.Tags{{"e", target.ID.Hex()}}, "delete")
	s := openTestStore(t, path)
	if err := s.SaveEvent(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEventAndApplyDeletion(ctx, deletion); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.SaveEvent(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Event(ctx, target.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replayed target = %v", err)
	}
}

func TestSQLiteStoreDeletionBeforeTargetRejectsLaterTargetAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite")
	owner := testPubKey2
	target := testEvent(25, owner, 10, 1, nil, "not-yet-seen")
	deletion := testEvent(26, owner, 20, 5, nostr.Tags{{"e", target.ID.Hex()}}, "delete first")
	s := openTestStore(t, path)
	if err := s.SaveEventAndApplyDeletion(ctx, deletion); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEvent(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Event(ctx, target.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("target accepted before restart: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.SaveEvent(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Event(ctx, target.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("target accepted after restart: %v", err)
	}
}

func TestSQLiteStoreForeignDeletionDoesNotCreateTombstone(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, filepath.Join(t.TempDir(), "relay.sqlite"))
	target := testEvent(30, nostr.NUMS, 10, 1, nil, "foreign")
	deletion := testEvent(31, testPubKey2, 20, 5, nostr.Tags{{"e", target.ID.Hex()}}, "foreign delete")
	if err := s.SaveEvent(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEventAndApplyDeletion(ctx, deletion); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEvent(ctx, target.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEvent(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Event(ctx, target.ID); err != nil {
		t.Fatalf("foreign target replay was suppressed: %v", err)
	}
}

func TestSQLiteStoreNIP98ReplayConsumptionPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite")
	id := nostr.ID{9}
	now := time.Unix(100, 0)
	s := openTestStore(t, path)
	if err := s.ConsumeNIP98Event(ctx, id, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.ConsumeNIP98Event(ctx, id, now.Add(time.Second), time.Minute); err == nil {
		t.Fatal("replayed NIP-98 event was accepted")
	}
}

func TestSQLiteStoreReplacesReplaceableAndAddressableEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t, filepath.Join(t.TempDir(), "relay.sqlite"))

	old := testEvent(1, nostr.NUMS, 10, 10000, nil, "old")
	newer := testEvent(2, nostr.NUMS, 20, 10000, nil, "new")
	for _, event := range []nostr.Event{old, newer} {
		if err := s.SaveEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	olderArrival := testEvent(6, nostr.NUMS, 5, 10000, nil, "older arrival")
	if err := s.SaveEvent(ctx, olderArrival); err != nil {
		t.Fatal(err)
	}
	assertEvents(t, s, nostr.Filter{Kinds: []nostr.Kind{10000}}, []nostr.Event{newer})

	a := testEvent(3, nostr.NUMS, 10, 30023, nostr.Tags{{"d", "a"}}, "a-old")
	aNew := testEvent(4, nostr.NUMS, 20, 30023, nostr.Tags{{"d", "a"}}, "a-new")
	b := testEvent(5, nostr.NUMS, 15, 30023, nostr.Tags{{"d", "b"}}, "b")
	for _, event := range []nostr.Event{a, aNew, b} {
		if err := s.SaveEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	assertEvents(t, s, nostr.Filter{Kinds: []nostr.Kind{30023}}, []nostr.Event{aNew, b})
}

func TestSQLiteStorePublisherAllowlist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t, filepath.Join(t.TempDir(), "relay.sqlite"))
	created := time.Unix(123, 0).UTC()

	for range 2 {
		if err := s.AllowPublisher(ctx, Publisher{PubKey: testPubKey2, Reason: "second", CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
		if err := s.AllowPublisher(ctx, Publisher{PubKey: nostr.NUMS, Reason: "first", CreatedAt: created.Add(time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	allowed, err := s.PublisherAllowed(ctx, testPubKey2)
	if err != nil || !allowed {
		t.Fatalf("PublisherAllowed() = %v, %v", allowed, err)
	}
	got, err := s.ListPublishers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Publisher{{PubKey: nostr.NUMS, Reason: "first", CreatedAt: created.Add(time.Second)}, {PubKey: testPubKey2, Reason: "second", CreatedAt: created}}
	if !slices.Equal(got, want) {
		t.Fatalf("ListPublishers() = %#v, want %#v", got, want)
	}

	for range 2 {
		if err := s.UnallowPublisher(ctx, testPubKey2); err != nil {
			t.Fatal(err)
		}
	}
	allowed, err = s.PublisherAllowed(ctx, testPubKey2)
	if err != nil || allowed {
		t.Fatalf("PublisherAllowed() = %v, %v", allowed, err)
	}
}

func TestSQLiteStorePersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite")
	event := testEvent(1, nostr.NUMS, 10, 1, nil, "persisted")
	pub := Publisher{PubKey: nostr.NUMS, Reason: "persisted", CreatedAt: time.Unix(123, 0).UTC()}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowPublisher(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got, err := s.Event(ctx, event.ID); err != nil || !reflect.DeepEqual(got, event) {
		t.Fatalf("Event() = %#v, %v", got, err)
	}
	if got, err := s.ListPublishers(ctx); err != nil || !slices.Equal(got, []Publisher{pub}) {
		t.Fatalf("ListPublishers() = %#v, %v", got, err)
	}
}

func countEventTags(t *testing.T, db *sql.DB, eventID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM event_tags WHERE event_id = ?`, eventID).Scan(&count); err != nil {
		t.Fatalf("count event_tags for %s: %v", eventID, err)
	}
	return count
}

func TestSQLiteStoreEventTagsCascadeOnDeleteAndReplace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTestStore(t, filepath.Join(t.TempDir(), "relay.sqlite"))

	ref := nostr.ID{0x42}

	// 1. DeleteEvent cascades event_tags.
	e1 := testEvent(1, nostr.NUMS, 10, 1, nostr.Tags{{"e", ref.Hex()}, {"p", testPubKey2.Hex()}}, "delete-me")
	if err := s.SaveEvent(ctx, e1); err != nil {
		t.Fatal(err)
	}
	if got := countEventTags(t, s.readDB, e1.ID.Hex()); got != 2 {
		t.Fatalf("event_tags count before DeleteEvent = %d, want 2", got)
	}
	if err := s.DeleteEvent(ctx, e1.ID); err != nil {
		t.Fatal(err)
	}
	if got := countEventTags(t, s.readDB, e1.ID.Hex()); got != 0 {
		t.Fatalf("event_tags count after DeleteEvent = %d, want 0", got)
	}

	// 2. Kind-5 SaveEventAndApplyDeletion cascades target's event_tags.
	e2 := testEvent(2, testPubKey2, 20, 1, nostr.Tags{{"e", ref.Hex()}}, "kind5-target")
	if err := s.SaveEvent(ctx, e2); err != nil {
		t.Fatal(err)
	}
	if got := countEventTags(t, s.readDB, e2.ID.Hex()); got != 1 {
		t.Fatalf("event_tags count before kind-5 deletion = %d, want 1", got)
	}
	del := testEvent(3, testPubKey2, 30, 5, nostr.Tags{{"e", e2.ID.Hex()}}, "delete")
	if err := s.SaveEventAndApplyDeletion(ctx, del); err != nil {
		t.Fatal(err)
	}
	if got := countEventTags(t, s.readDB, e2.ID.Hex()); got != 0 {
		t.Fatalf("event_tags count after kind-5 deletion = %d, want 0", got)
	}

	// 3. Replaceable (kind 10000) replacement cascades old event's event_tags.
	rOld := testEvent(4, nostr.NUMS, 40, 10000, nostr.Tags{{"p", testPubKey2.Hex()}}, "old-replaceable")
	rNew := testEvent(5, nostr.NUMS, 50, 10000, nostr.Tags{{"p", nostr.NUMS.Hex()}}, "new-replaceable")
	if err := s.SaveEvent(ctx, rOld); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEvent(ctx, rNew); err != nil {
		t.Fatal(err)
	}
	if got := countEventTags(t, s.readDB, rOld.ID.Hex()); got != 0 {
		t.Fatalf("event_tags count for replaced kind-10000 = %d, want 0", got)
	}
	if got := countEventTags(t, s.readDB, rNew.ID.Hex()); got != 1 {
		t.Fatalf("event_tags count for new kind-10000 = %d, want 1", got)
	}

	// 4. Addressable (kind 30023) replacement cascades old event's event_tags.
	aOld := testEvent(6, nostr.NUMS, 60, 30023, nostr.Tags{{"d", "slug"}, {"t", "old"}}, "old-addr")
	aNew := testEvent(7, nostr.NUMS, 70, 30023, nostr.Tags{{"d", "slug"}, {"t", "new"}}, "new-addr")
	if err := s.SaveEvent(ctx, aOld); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEvent(ctx, aNew); err != nil {
		t.Fatal(err)
	}
	if got := countEventTags(t, s.readDB, aOld.ID.Hex()); got != 0 {
		t.Fatalf("event_tags count for replaced kind-30023 = %d, want 0", got)
	}
	if got := countEventTags(t, s.readDB, aNew.ID.Hex()); got != 2 {
		t.Fatalf("event_tags count for new kind-30023 = %d, want 2", got)
	}
}

func TestSQLiteStoreBackfillsEventTagsOnLegacyDB(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")

	refID := nostr.ID{0x55}
	e1 := testEvent(1, nostr.NUMS, 100, 1, nostr.Tags{
		{"e", refID.Hex()},
		{"p", testPubKey2.Hex()},
		{"client", "nostter"},
	}, "legacy-one")
	e2 := testEvent(2, testPubKey2, 200, 1, nostr.Tags{
		{"p", nostr.NUMS.Hex()},
		{"client", "nostter"},
	}, "legacy-two")

	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `
		CREATE TABLE events (
			id TEXT PRIMARY KEY,
			pubkey TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			kind INTEGER NOT NULL,
			event_json TEXT NOT NULL
		);
	`); err != nil {
		_ = rawDB.Close()
		t.Fatal(err)
	}
	for _, ev := range []nostr.Event{e1, e2} {
		encoded, err := json.Marshal(ev)
		if err != nil {
			_ = rawDB.Close()
			t.Fatal(err)
		}
		if _, err := rawDB.ExecContext(ctx,
			`INSERT INTO events (id, pubkey, created_at, kind, event_json) VALUES (?, ?, ?, ?, ?)`,
			ev.ID.Hex(), ev.PubKey.Hex(), int64(ev.CreatedAt), int64(ev.Kind), string(encoded),
		); err != nil {
			_ = rawDB.Close()
			t.Fatal(err)
		}
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	assertEvents(t, s, nostr.Filter{Tags: nostr.TagMap{"e": {refID.Hex()}}}, []nostr.Event{e1})
	assertEvents(t, s, nostr.Filter{Tags: nostr.TagMap{"p": {testPubKey2.Hex()}}}, []nostr.Event{e1})
	assertEvents(t, s, nostr.Filter{Tags: nostr.TagMap{"p": {nostr.NUMS.Hex()}}}, []nostr.Event{e2})

	var clientTagCount int
	if err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_tags WHERE tag_name = 'client'`).Scan(&clientTagCount); err != nil {
		t.Fatal(err)
	}
	if clientTagCount != 0 {
		t.Fatalf("multi-char tag 'client' rows = %d, want 0", clientTagCount)
	}

	var backfilled string
	if err := s.readDB.QueryRowContext(ctx, `SELECT value FROM relay_meta WHERE key = 'event_tags_backfilled'`).Scan(&backfilled); err != nil {
		t.Fatal(err)
	}
	if backfilled != "1" {
		t.Fatalf("relay_meta event_tags_backfilled = %q, want %q", backfilled, "1")
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertEvents(t, reopened, nostr.Filter{Tags: nostr.TagMap{"e": {refID.Hex()}}}, []nostr.Event{e1})
	assertEvents(t, reopened, nostr.Filter{Tags: nostr.TagMap{"p": {nostr.NUMS.Hex()}}}, []nostr.Event{e2})
}

func TestSQLiteStoreConcurrentReadsAndWrites(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTestStore(t, filepath.Join(t.TempDir(), "concurrent.sqlite"))

	targetID := nostr.ID{0x77}
	seedIDs := make([]nostr.ID, 50)
	for i := range 50 {
		ev := testEvent(byte(i+1), testPubKey2, nostr.Timestamp(1000+i), 7, nostr.Tags{
			{"e", targetID.Hex()},
		}, "seed")
		seedIDs[i] = ev.ID
		if err := s.SaveEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	errCh := make(chan error, 16*50+2*25)
	var wg sync.WaitGroup

	for r := range 16 {
		readerID := r
		wg.Go(func() {
			for i := range 50 {
				var filter nostr.Filter
				switch (readerID + i) % 3 {
				case 0:
					filter = nostr.Filter{IDs: []nostr.ID{seedIDs[i%len(seedIDs)]}}
				case 1:
					filter = nostr.Filter{
						Authors: []nostr.PubKey{testPubKey2},
						Kinds:   []nostr.Kind{7},
						Tags:    nostr.TagMap{"e": {targetID.Hex()}},
						Limit:   20,
					}
				case 2:
					filter = nostr.Filter{
						Authors: []nostr.PubKey{testPubKey2},
						Kinds:   []nostr.Kind{7},
						Limit:   20,
					}
				}
				seq, err := s.QueryEvents(ctx, filter)
				if err != nil {
					errCh <- err
					return
				}
				_ = slices.Collect(seq)
			}
		})
	}

	for w := range 2 {
		writerID := w
		wg.Go(func() {
			for i := range 25 {
				var id nostr.ID
				id[30] = byte(writerID + 1)
				id[31] = byte(i + 1)
				ev := nostr.Event{
					ID:        id,
					PubKey:    testPubKey2,
					CreatedAt: nostr.Timestamp(2000 + writerID*100 + i),
					Kind:      7,
					Tags:      nostr.Tags{{"e", targetID.Hex()}},
					Content:   "concurrent-write",
				}
				if err := s.SaveEvent(ctx, ev); err != nil {
					errCh <- err
					return
				}
			}
		})
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent operation error: %v", err)
	}
}

func openTestStore(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testEvent(id byte, pubkey nostr.PubKey, created nostr.Timestamp, kind nostr.Kind, tags nostr.Tags, content string) nostr.Event {
	var eventID nostr.ID
	eventID[31] = id
	if tags == nil {
		tags = nostr.Tags{}
	}
	return nostr.Event{ID: eventID, PubKey: pubkey, CreatedAt: created, Kind: kind, Tags: tags, Content: content}
}

func assertEvents(t *testing.T, s *SQLiteStore, filter nostr.Filter, want []nostr.Event) {
	t.Helper()
	seq, err := s.QueryEvents(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Collect(seq)
	slices.SortFunc(got, func(a, b nostr.Event) int { return int(a.ID[31]) - int(b.ID[31]) })
	slices.SortFunc(want, func(a, b nostr.Event) int { return int(a.ID[31]) - int(b.ID[31]) })
	if !slices.EqualFunc(got, want, eventsEqual) {
		t.Fatalf("QueryEvents() = %#v, want %#v", got, want)
	}
}

func eventsEqual(a, b nostr.Event) bool { return reflect.DeepEqual(a, b) }
