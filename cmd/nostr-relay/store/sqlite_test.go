package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
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
	source, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "sqlite.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"ExecContext", "QueryContext", "QueryRowContext"} {
		if regexp.MustCompile(`\b` + forbidden + `\b`).Match(source) {
			t.Errorf("sqlite.go contains forbidden database call %s", forbidden)
		}
	}
	rawSQL := regexp.MustCompile(`(?i)[\x60\x22][^\x60\x22]*(SELECT|INSERT|UPDATE|DELETE)\b`)
	if match := rawSQL.Find(source); match != nil {
		t.Errorf("sqlite.go contains raw SQL string %q", match)
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
	if _, err := s.db.Exec(`CREATE TRIGGER fail_event_delete BEFORE DELETE ON events BEGIN SELECT RAISE(FAIL, 'injected delete failure'); END`); err != nil {
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
