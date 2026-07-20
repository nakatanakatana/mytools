package store

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

var testScope = SourceScope{Provider: "bluesky", Account: "did:plc:test"}

func testRef(uri string) SourceRef { return SourceRef{Scope: testScope, URI: uri} }

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
	parsed, err := parser.ParseFile(token.NewFileSet(), "sqlite.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	rawSQL := regexp.MustCompile(`(?is)^\s*(WITH\s+\w+\s+AS\b|SELECT\s+.+\s+FROM\b|INSERT\s+(INTO|OR)\b|UPDATE\s+\w+\s+SET\b|DELETE\s+FROM\b)`)
	ast.Inspect(parsed, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Errorf("unquote sqlite.go string literal: %v", err)
			return true
		}
		if rawSQL.MatchString(value) {
			t.Errorf("sqlite.go contains raw SQL string %q", value)
		}
		return true
	})
}

func saveTestMapping(ctx context.Context, s SQLiteStore, mapping EventMapping) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO bridge_events(provider, source_account, source_uri, nostr_event_id, source_kind, author_pubkey, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?)`, mapping.Source.Scope.Provider, mapping.Source.Scope.Account, mapping.Source.URI, mapping.NostrEventID, mapping.SourceKind, mapping.AuthorPubKey, mapping.UpdatedAt)
	return err
}

func reconciliationTestEvent(t *testing.T, uri string) EventEnqueueRequest {
	t.Helper()
	event := nostr.Event{CreatedAt: nostr.Now(), Kind: 1, Content: uri}
	if err := event.Sign(nostr.Generate()); err != nil {
		t.Fatal(err)
	}
	return EventEnqueueRequest{Mapping: EventMapping{Source: testRef(uri), NostrEventID: event.ID.Hex(), AuthorPubKey: event.PubKey.Hex()}, Event: OutboxRequest{AggregateKey: event.PubKey.Hex(), Operation: OutboxPublishEvent, PubKey: event.PubKey.Hex(), Payload: event.String(), AvailableAt: time.Now()}}
}

func signedStoreEvent(t *testing.T, sk nostr.SecretKey, kind nostr.Kind, content string) nostr.Event {
	t.Helper()
	e := nostr.Event{CreatedAt: nostr.Now(), Kind: kind, Content: content}
	if err := e.Sign(sk); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestReconcileAtomicallyReplacesTargetsAndQueuesWholeIdempotentBatch(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "reconcile.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	if err := s.ReplaceSyncTargets(ctx, testScope, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	a, b := reconciliationTestEvent(t, "a"), reconciliationTestEvent(t, "b")
	bad := b
	bad.Event.PubKey = a.Event.PubKey
	if err := s.Reconcile(ctx, ReconciliationRequest{Scope: testScope, Targets: []string{"new"}, Events: []EventEnqueueRequest{a, bad}, Limit: 4}); !errors.Is(err, ErrAuthorMismatch) {
		t.Fatalf("failure = %v", err)
	}
	if targets, _ := s.SyncTargets(ctx, testScope); !slices.Equal(targets, []string{"old"}) {
		t.Fatalf("targets after failure = %#v", targets)
	}
	if count, _ := s.OutboxCount(ctx); count != 0 {
		t.Fatalf("count after failure = %d", count)
	}
	request := ReconciliationRequest{Scope: testScope, Targets: []string{"new"}, Events: []EventEnqueueRequest{a, b}, Limit: 4}
	if err := s.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if count, _ := s.OutboxCount(ctx); count != 4 {
		t.Fatalf("exact-limit count = %d", count)
	}
	if err := s.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if count, _ := s.OutboxCount(ctx); count != 4 {
		t.Fatalf("retry count = %d", count)
	}
}

func TestReconcileBatchRollsBackProviderTargetsWhenOwnerMappingFails(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "batch-rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	provider := SourceScope{Provider: "bluesky", Account: "owner"}
	owner := SourceScope{Provider: "bridge-owner", Account: "home"}
	if err := s.ReplaceSyncTargets(ctx, provider, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TEMP TRIGGER fail_owner_mapping BEFORE INSERT ON bridge_events WHEN NEW.provider = 'bridge-owner' BEGIN SELECT RAISE(ABORT, 'owner mapping failed'); END`); err != nil {
		t.Fatal(err)
	}
	providerEvent := reconciliationTestEventForScope(t, provider, "provider-profile")
	ownerEvent := reconciliationTestEventForScope(t, owner, "owner-profile")
	before, _ := s.OutboxCount(ctx)
	err = s.ReconcileBatch(ctx, ReconciliationBatchRequest{TargetScope: provider, Targets: []string{"new"}, EventScopes: []SourceScope{provider, owner}, Events: []EventEnqueueRequest{providerEvent, ownerEvent}, Limit: 100})
	if err == nil {
		t.Fatal("expected injected owner mapping failure")
	}
	if targets, _ := s.SyncTargets(ctx, provider); !slices.Equal(targets, []string{"old"}) {
		t.Fatalf("targets = %#v", targets)
	}
	for _, request := range []EventEnqueueRequest{providerEvent, ownerEvent} {
		if _, err := s.EventMappingBySourceURI(ctx, request.Mapping.Source); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("partial mapping %v: %v", request.Mapping.Source, err)
		}
	}
	if after, _ := s.OutboxCount(ctx); after != before {
		t.Fatalf("outbox count = %d, want %d", after, before)
	}
}

func reconciliationTestEventForScope(t *testing.T, scope SourceScope, uri string) EventEnqueueRequest {
	t.Helper()
	request := reconciliationTestEvent(t, uri)
	request.Mapping.Source = SourceRef{Scope: scope, URI: uri}
	return request
}

func TestEnqueueEventEnforcesHardLimitAtomically(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	sk := nostr.Generate()
	e := signedStoreEvent(t, sk, 1, "hard limit")
	req := func(uri, pubkey string) EventEnqueueRequest {
		return EventEnqueueRequest{Mapping: EventMapping{Source: testRef(uri), NostrEventID: e.ID.Hex(), SourceKind: "post", AuthorPubKey: pubkey}, Event: OutboxRequest{AggregateKey: pubkey, Operation: OutboxPublishEvent, PubKey: pubkey, Payload: e.String(), AvailableAt: time.Now()}, Limit: 2}
	}
	pubkey := e.PubKey.Hex()
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, uri := range []string{"one", "two"} {
		go func(uri string) { <-start; results <- s.EnqueueEvent(ctx, req(uri, pubkey)) }(uri)
	}
	close(start)
	a, b := <-results, <-results
	if (a == nil) == (b == nil) {
		t.Fatalf("results = %v, %v; want one success", a, b)
	}
	if count, _ := s.OutboxCount(ctx); count != 2 {
		t.Fatalf("outbox count = %d, want hard limit 2", count)
	}
}

func TestEnqueueEventRollsBackEveryMutationOnFailure(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	pubkey := strings.Repeat("2", 64)
	err = s.EnqueueEvent(ctx, EventEnqueueRequest{Mapping: EventMapping{Source: testRef("source"), NostrEventID: strings.Repeat("a", 64), SourceKind: "post", AuthorPubKey: pubkey}, Event: OutboxRequest{AggregateKey: pubkey, Operation: "invalid", PubKey: pubkey, Payload: `{}`, AvailableAt: time.Now()}, Limit: 10, Cursor: &CursorUpdate{Name: "cursor", Value: "9"}})
	if err == nil {
		t.Fatal("expected injected constraint failure")
	}
	if count, _ := s.OutboxCount(ctx); count != 0 {
		t.Fatalf("outbox count = %d", count)
	}
	if _, err := s.EventMappingBySourceURI(ctx, testRef("source")); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("mapping error = %v", err)
	}
	if _, err := s.Cursor(ctx, testScope, "cursor"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cursor error = %v", err)
	}
}

func TestEnqueueUpdateChecksCapacityBeforeReplacingMapping(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "update-limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	sk := nostr.Generate()
	deletion := signedStoreEvent(t, sk, 5, "delete")
	replacement := signedStoreEvent(t, sk, 1, "replace")
	pubkey := replacement.PubKey.Hex()
	oldID := strings.Repeat("a", 64)
	newID := replacement.ID.Hex()
	_ = saveTestMapping(ctx, s, EventMapping{Source: testRef("source"), NostrEventID: oldID, SourceKind: "post", AuthorPubKey: pubkey})
	_ = s.SetPublisherRegistered(ctx, pubkey, time.Now())
	_ = enqueueTestOutbox(ctx, s, OutboxRequest{AggregateKey: "other", Operation: OutboxPublishEvent, PubKey: pubkey, Payload: `{}`, AvailableAt: time.Now()})
	err = s.EnqueueUpdate(ctx, UpdateEnqueueRequest{Mapping: EventMapping{Source: testRef("source"), NostrEventID: newID, SourceKind: "post", AuthorPubKey: pubkey}, Deletion: OutboxRequest{AggregateKey: pubkey, Operation: OutboxPublishEvent, PubKey: pubkey, Payload: deletion.String(), AvailableAt: time.Now()}, Replacement: OutboxRequest{AggregateKey: pubkey, Operation: OutboxPublishEvent, PubKey: pubkey, Payload: replacement.String(), AvailableAt: time.Now()}, Limit: 2})
	if !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("EnqueueUpdate() = %v", err)
	}
	mapping, _ := s.EventMappingBySourceURI(ctx, testRef("source"))
	if mapping.NostrEventID != oldID {
		t.Fatalf("mapping = %s", mapping.NostrEventID)
	}
	if count, _ := s.OutboxCount(ctx); count != 1 {
		t.Fatalf("outbox count = %d", count)
	}
}

func TestEnqueueUpdateOrdersDeletionThenReplacementAndCommitsMetadata(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "update.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	sk := nostr.Generate()
	deletion := signedStoreEvent(t, sk, 5, "delete")
	replacement := signedStoreEvent(t, sk, 1, "replace")
	pubkey := replacement.PubKey.Hex()
	_ = s.SetPublisherRegistered(ctx, pubkey, time.Now())
	req := UpdateEnqueueRequest{Mapping: EventMapping{Source: testRef("source"), NostrEventID: replacement.ID.Hex(), SourceKind: "post", AuthorPubKey: pubkey}, Deletion: OutboxRequest{AggregateKey: pubkey, Operation: OutboxPublishEvent, PubKey: pubkey, Payload: deletion.String(), AvailableAt: time.Now()}, Replacement: OutboxRequest{AggregateKey: pubkey, Operation: OutboxPublishEvent, PubKey: pubkey, Payload: replacement.String(), AvailableAt: time.Now()}, SourceOperation: "cid:new", Limit: 2, Cursor: &CursorUpdate{Name: "cursor", Value: "8"}}
	if err := s.EnqueueUpdate(ctx, req); err != nil {
		t.Fatal(err)
	}
	items, _ := s.ClaimOutbox(ctx, time.Now().Add(time.Second), time.Minute, 1)
	if len(items) != 1 || items[0].Payload != deletion.String() {
		t.Fatalf("first = %#v", items)
	}
	_ = s.CompleteOutbox(ctx, items[0].ID, items[0].ClaimToken, time.Now())
	items, _ = s.ClaimOutbox(ctx, time.Now().Add(time.Second), time.Minute, 1)
	if len(items) != 1 || items[0].Payload != replacement.String() {
		t.Fatalf("second = %#v", items)
	}
	if op, _ := s.SourceOperationBySourceURI(ctx, testRef("source")); op != "cid:new" {
		t.Fatalf("operation = %q", op)
	}
	if cursor, _ := s.Cursor(ctx, testScope, "cursor"); cursor != "8" {
		t.Fatalf("cursor = %s", cursor)
	}
}

func TestAuthorMismatchRejectsDomainMutations(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "authors.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	now := time.Now()
	_ = saveTestMapping(ctx, s, EventMapping{Source: testRef("old"), NostrEventID: strings.Repeat("c", 64), AuthorPubKey: a})
	tests := []struct {
		name string
		run  func() error
	}{
		{"legacy", func() error {
			return s.SaveEventAndEnqueue(ctx, EventMapping{Source: testRef("new"), AuthorPubKey: a}, OutboxRequest{AggregateKey: b, Operation: OutboxPublishEvent, PubKey: b, Payload: `{}`, AvailableAt: now})
		}},
		{"event", func() error {
			return s.EnqueueEvent(ctx, EventEnqueueRequest{Mapping: EventMapping{Source: testRef("new"), AuthorPubKey: a}, Event: OutboxRequest{AggregateKey: b, Operation: OutboxPublishEvent, PubKey: b, Payload: `{}`, AvailableAt: now}, Limit: 10})
		}},
		{"update", func() error {
			return s.EnqueueUpdate(ctx, UpdateEnqueueRequest{Mapping: EventMapping{Source: testRef("old"), AuthorPubKey: a}, Deletion: OutboxRequest{AggregateKey: a, PubKey: a, Payload: `{}`}, Replacement: OutboxRequest{AggregateKey: b, PubKey: b, Payload: `{}`}, Limit: 10})
		}},
		{"delete", func() error {
			return s.EnqueueDelete(ctx, DeleteEnqueueRequest{Source: testRef("old"), Event: OutboxRequest{AggregateKey: b, PubKey: b, Payload: `{}`}, Limit: 10})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, ErrAuthorMismatch) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if count, _ := s.OutboxCount(ctx); count != 0 {
		t.Fatalf("outbox count = %d", count)
	}
	mapping, _ := s.EventMappingBySourceURI(ctx, testRef("old"))
	if mapping.AuthorPubKey != a {
		t.Fatal("mapping mutated")
	}
}

func TestMappingEventIDMustMatchPublishedEvent(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "mapping-id.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	sk := nostr.Generate()
	event := signedStoreEvent(t, sk, 1, "payload")
	pubkey := event.PubKey.Hex()
	badMapping := EventMapping{Source: testRef("source"), NostrEventID: strings.Repeat("f", 64), AuthorPubKey: pubkey}
	request := OutboxRequest{AggregateKey: pubkey, Operation: OutboxPublishEvent, PubKey: pubkey, Payload: event.String(), AvailableAt: time.Now()}
	checks := []struct {
		name string
		run  func() error
	}{
		{"save", func() error { return s.SaveEventAndEnqueue(ctx, badMapping, request) }},
		{"enqueue", func() error {
			return s.EnqueueEvent(ctx, EventEnqueueRequest{Mapping: badMapping, Event: request, Limit: 10})
		}},
		{"reconcile", func() error {
			return s.Reconcile(ctx, ReconciliationRequest{Scope: testScope, Events: []EventEnqueueRequest{{Mapping: badMapping, Event: request}}, Limit: 10})
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, ErrInvalidOutboxPayload) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	replacement := signedStoreEvent(t, sk, 1, "replacement")
	deletion := signedStoreEvent(t, sk, 5, "deletion")
	badMapping.NostrEventID = event.ID.Hex()
	err = s.EnqueueUpdate(ctx, UpdateEnqueueRequest{Mapping: badMapping, Deletion: OutboxRequest{AggregateKey: pubkey, Operation: OutboxPublishEvent, PubKey: pubkey, Payload: deletion.String()}, Replacement: OutboxRequest{AggregateKey: pubkey, Operation: OutboxPublishEvent, PubKey: pubkey, Payload: replacement.String()}, Limit: 10})
	if !errors.Is(err, ErrInvalidOutboxPayload) {
		t.Fatalf("update error = %v", err)
	}
}

func TestSQLiteStoreSavesMappingAndEnqueuesAtomically(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	signed := signedStoreEvent(t, nostr.Generate(), 1, "event-1")
	event := EventMapping{Source: testRef("at://did:plc:alice/app.bsky.feed.post/1"), NostrEventID: signed.ID.Hex(), SourceKind: "app.bsky.feed.post", AuthorPubKey: signed.PubKey.Hex(), UpdatedAt: 10}
	if err := s.SaveEventAndEnqueue(ctx, event, OutboxRequest{AggregateKey: signed.PubKey.Hex(), Operation: OutboxPublishEvent, PubKey: signed.PubKey.Hex(), Payload: signed.String(), AvailableAt: time.Unix(20, 0)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.EventMappingBySourceURI(ctx, testRef(event.Source.URI))
	if err != nil {
		t.Fatal(err)
	}
	if got != event {
		t.Fatalf("EventBySourceURI() = %#v, want %#v", got, event)
	}
	items, err := s.ClaimOutbox(ctx, time.Unix(20, 0), time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Payload != signed.String() || items[0].Sequence != 1 {
		t.Fatalf("ClaimOutbox() = %#v", items)
	}
}

func TestSQLiteStoreRollsBackMappingWhenEnqueueFails(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	event := EventMapping{Source: testRef("at://did:plc:alice/app.bsky.feed.post/bad"), NostrEventID: "bad", SourceKind: "post", AuthorPubKey: "alice"}
	err = s.SaveEventAndEnqueue(ctx, event, OutboxRequest{AggregateKey: event.Source.URI, Operation: OutboxOperation("invalid"), AvailableAt: time.Unix(1, 0)})
	if err == nil {
		t.Fatal("SaveEventAndEnqueue() succeeded with invalid operation")
	}
	if _, err := s.EventMappingBySourceURI(ctx, testRef(event.Source.URI)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("EventBySourceURI() error = %v, want sql.ErrNoRows", err)
	}
}

func TestSQLiteStoreOrdersEligibleOutboxPerAggregate(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	for _, req := range []OutboxRequest{
		{AggregateKey: "a", Operation: OutboxAllowPublisher, PubKey: "a", AvailableAt: time.Unix(30, 0)},
		{AggregateKey: "a", Operation: OutboxPublishEvent, PubKey: "a", AvailableAt: time.Unix(10, 0)},
		{AggregateKey: "b", Operation: OutboxPublishEvent, PubKey: "b", AvailableAt: time.Unix(20, 0)},
	} {
		if err := enqueueTestOutbox(ctx, s, req); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.ClaimOutbox(ctx, time.Unix(40, 0), time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].AggregateKey != "b" || items[1].AggregateKey != "a" || items[1].Sequence != 1 {
		t.Fatalf("ClaimOutbox() = %#v", items)
	}
}

func TestSQLiteStoreCompletesAndRetriesClaimedOutbox(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	if err := enqueueTestOutbox(ctx, s, OutboxRequest{AggregateKey: "a", Operation: OutboxPublishEvent, Payload: "payload", AvailableAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	items, err := s.ClaimOutbox(ctx, time.Unix(2, 0), time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RetryOutbox(ctx, items[0].ID, items[0].ClaimToken, time.Unix(3, 0), time.Unix(4, 0), "temporary"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteOutbox(ctx, items[0].ID, "", time.Unix(3, 0)); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("unowned CompleteOutbox() = %v, want ErrClaimLost", err)
	}
	items, err = s.ClaimOutbox(ctx, time.Unix(4, 0), time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Attempts != 1 || items[0].LastError != "temporary" {
		t.Fatalf("retried item = %#v", items)
	}
	if err := s.CompleteOutbox(ctx, items[0].ID, items[0].ClaimToken, time.Unix(5, 0)); err != nil {
		t.Fatal(err)
	}
	if err := enqueueTestOutbox(ctx, s, OutboxRequest{AggregateKey: "a", Operation: OutboxPublishEvent, AvailableAt: time.Unix(5, 0)}); err != nil {
		t.Fatal(err)
	}
	items, err = s.ClaimOutbox(ctx, time.Unix(5, 0), time.Minute, 1)
	if err != nil || len(items) != 1 || items[0].Sequence != 2 {
		t.Fatalf("next aggregate sequence = %#v, %v", items, err)
	}
	if err := s.CompleteOutbox(ctx, items[0].ID, items[0].ClaimToken, time.Unix(6, 0)); err != nil {
		t.Fatal(err)
	}
	if count, err := s.OutboxCount(ctx); err != nil || count != 0 {
		t.Fatalf("OutboxCount() = %d, %v", count, err)
	}
}

func TestSQLiteStoreReclaimsExpiredLeaseAndRejectsStaleOwner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, closer, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueueTestOutbox(ctx, s, OutboxRequest{AggregateKey: "a", Operation: OutboxPublishEvent, AvailableAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := enqueueTestOutbox(ctx, s, OutboxRequest{AggregateKey: "a", Operation: OutboxPublishEvent, AvailableAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimOutbox(ctx, time.Unix(2, 0), time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	s, closer, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	before, err := s.ClaimOutbox(ctx, time.Unix(61, 0), time.Minute, 1)
	if err != nil || len(before) != 0 {
		t.Fatalf("claim before expiry = %#v, %v", before, err)
	}
	if err := s.CompleteOutbox(ctx, first[0].ID, first[0].ClaimToken, time.Unix(62, 0)); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("expired CompleteOutbox() = %v, want ErrClaimLost", err)
	}
	if err := s.RetryOutbox(ctx, first[0].ID, first[0].ClaimToken, time.Unix(62, 0), time.Unix(70, 0), "expired"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("expired RetryOutbox() = %v, want ErrClaimLost", err)
	}
	reclaimed, err := s.ClaimOutbox(ctx, time.Unix(62, 0), time.Minute, 1)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].ClaimToken == first[0].ClaimToken {
		t.Fatalf("reclaimed = %#v, %v", reclaimed, err)
	}
	if err := s.CompleteOutbox(ctx, first[0].ID, first[0].ClaimToken, time.Unix(63, 0)); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("stale CompleteOutbox() = %v, want ErrClaimLost", err)
	}
	if err := s.RetryOutbox(ctx, first[0].ID, first[0].ClaimToken, time.Unix(63, 0), time.Unix(70, 0), "stale"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("stale RetryOutbox() = %v, want ErrClaimLost", err)
	}
	if err := s.CompleteOutbox(ctx, reclaimed[0].ID, reclaimed[0].ClaimToken, time.Unix(63, 0)); err != nil {
		t.Fatal(err)
	}
	next, err := s.ClaimOutbox(ctx, time.Unix(62, 0), time.Minute, 1)
	if err != nil || len(next) != 1 || next[0].Sequence != 2 {
		t.Fatalf("next aggregate item = %#v, %v", next, err)
	}
}

func TestSQLiteStoreRejectsNonPositiveClaimLease(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	for _, lease := range []time.Duration{0, -time.Second} {
		if _, err := s.ClaimOutbox(ctx, time.Unix(1, 0), lease, 1); !errors.Is(err, ErrInvalidLease) {
			t.Fatalf("ClaimOutbox(lease=%v) error = %v, want ErrInvalidLease", lease, err)
		}
	}
}

func TestSQLiteStoreConcurrentClaimsHaveOneOwner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	first, firstCloser, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstCloser.Close() })
	second, secondCloser, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondCloser.Close() })
	if err := enqueueTestOutbox(ctx, first, OutboxRequest{AggregateKey: "a", Operation: OutboxPublishEvent, AvailableAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan []OutboxItem, 2)
	errs := make(chan error, 2)
	claim := func(s SQLiteStore) {
		<-start
		items, err := s.ClaimOutbox(ctx, time.Unix(2, 0), time.Minute, 1)
		results <- items
		errs <- err
	}
	go claim(first)
	go claim(second)
	close(start)
	owners := 0
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		owners += len(<-results)
	}
	if owners != 1 {
		t.Fatalf("claim owners = %d, want 1", owners)
	}
}

func enqueueTestOutbox(ctx context.Context, s SQLiteStore, request OutboxRequest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := enqueueOutbox(ctx, s.queries.WithTx(tx), request); err != nil {
		return err
	}
	return tx.Commit()
}

func TestSQLiteStorePublisherRegistrationAndSyncTargets(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	if err := s.SetPublisherRegistered(ctx, "pubkey", time.Unix(10, 0)); err != nil {
		t.Fatal(err)
	}
	registered, err := s.PublisherRegistered(ctx, "pubkey")
	if err != nil || !registered {
		t.Fatalf("PublisherRegistered() = %v, %v", registered, err)
	}
	if err := s.ClearPublisherRegistration(ctx, "pubkey"); err != nil {
		t.Fatal(err)
	}
	registered, err = s.PublisherRegistered(ctx, "pubkey")
	if err != nil || registered {
		t.Fatalf("PublisherRegistered() after clear = %v, %v", registered, err)
	}
	if err := s.ReplaceSyncTargets(ctx, testScope, []string{"did:plc:b", "did:plc:a", "did:plc:a"}); err != nil {
		t.Fatal(err)
	}
	targets, err := s.SyncTargets(ctx, testScope)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(targets, []string{"did:plc:a", "did:plc:b"}) {
		t.Fatalf("SyncTargets() = %#v", targets)
	}
	if err := s.ReplaceSyncTargets(ctx, testScope, []string{"did:plc:c"}); err != nil {
		t.Fatal(err)
	}
	targets, err = s.SyncTargets(ctx, testScope)
	if err != nil || !slices.Equal(targets, []string{"did:plc:c"}) {
		t.Fatalf("replaced SyncTargets() = %#v, %v", targets, err)
	}
}

func TestSQLiteStoreSavesSourceOperationIdentity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, closer, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	uri := "at://did:plc:alice/app.bsky.feed.post/123"
	if err := store.SaveSourceOperation(ctx, testRef(uri), "cid:bafy-update"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteEventBySourceURI(ctx, testRef(uri)); err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	store, closer, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	got, err := store.SourceOperationBySourceURI(ctx, testRef(uri))
	if err != nil {
		t.Fatal(err)
	}
	if got != "cid:bafy-update" {
		t.Fatalf("SourceOperationBySourceURI() = %q, want %q", got, "cid:bafy-update")
	}
}

func TestSQLiteStoreUpdatesCursor(t *testing.T) {
	ctx := context.Background()
	store, closer, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	if err := store.SaveCursor(ctx, testScope, "jetstream", "10"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCursor(ctx, testScope, "jetstream", "20"); err != nil {
		t.Fatal(err)
	}

	got, err := store.Cursor(ctx, testScope, "jetstream")
	if err != nil {
		t.Fatal(err)
	}
	if got != "20" {
		t.Fatalf("Cursor() = %s, want 20", got)
	}
}

func TestSQLiteStoreSavesFindsAndDeletesOAuthSession(t *testing.T) {
	ctx := context.Background()
	store, closer, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	want := OAuthSession{
		State:            "oauth-state",
		EncryptedPayload: []byte("encrypted-session"),
		ExpiresAt:        1_700_000_600,
	}
	if err := store.SaveOAuthSession(ctx, testScope, want); err != nil {
		t.Fatal(err)
	}

	got, err := store.OAuthSessionByState(ctx, testScope, want.State)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != want.State || string(got.EncryptedPayload) != string(want.EncryptedPayload) || got.ExpiresAt != want.ExpiresAt {
		t.Fatalf("OAuthSessionByState() = %#v, want %#v", got, want)
	}

	if err := store.DeleteOAuthSession(ctx, testScope, want.State); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OAuthSessionByState(ctx, testScope, want.State); err == nil {
		t.Fatal("OAuthSessionByState() succeeded after deletion")
	} else if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("OAuthSessionByState() error = %v, want sql.ErrNoRows", err)
	}
}

func TestSQLiteStoreUpdatesOAuthToken(t *testing.T) {
	ctx := context.Background()
	store, closer, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	first := OAuthToken{AccountDID: "did:plc:alice", EncryptedPayload: []byte("first"), UpdatedAt: 1}
	second := OAuthToken{AccountDID: first.AccountDID, EncryptedPayload: []byte("second"), UpdatedAt: 2}
	if err := store.SaveOAuthToken(ctx, testScope, first); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveOAuthToken(ctx, testScope, second); err != nil {
		t.Fatal(err)
	}

	got, err := store.OAuthTokenByAccountDID(ctx, testScope, first.AccountDID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountDID != second.AccountDID || string(got.EncryptedPayload) != string(second.EncryptedPayload) || got.UpdatedAt != second.UpdatedAt {
		t.Fatalf("OAuthTokenByAccountDID() = %#v, want %#v", got, second)
	}
}

func TestSQLiteStoreSeparatesSourceStateByScope(t *testing.T) {
	ctx := context.Background()
	s, closer, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	b := SourceScope{Provider: "bluesky", Account: "owner"}
	bOther := SourceScope{Provider: "bluesky", Account: "other-owner"}
	m := SourceScope{Provider: "mastodon", Account: "owner"}

	for scope, value := range map[SourceScope]string{b: "10", bOther: "15", m: "20"} {
		if err := s.SaveCursor(ctx, scope, "stream", value); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveOAuthSession(ctx, scope, OAuthSession{State: "same", EncryptedPayload: []byte(value)}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveOAuthToken(ctx, scope, OAuthToken{AccountDID: "owner", EncryptedPayload: []byte(value)}); err != nil {
			t.Fatal(err)
		}
		if err := s.ReplaceSyncTargets(ctx, scope, []string{value}); err != nil {
			t.Fatal(err)
		}
		ref := SourceRef{Scope: scope, URI: "same"}
		if err := s.SaveSourceOperation(ctx, ref, value); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveEventMapping(ctx, EventMapping{Source: ref, NostrEventID: value, SourceKind: "post", AuthorPubKey: value}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.Cursor(ctx, b, "stream"); got != "10" {
		t.Fatalf("cursor = %q", got)
	}
	if got, _ := s.OAuthSessionByState(ctx, b, "same"); string(got.EncryptedPayload) != "10" {
		t.Fatalf("session = %q", got.EncryptedPayload)
	}
	if got, _ := s.OAuthTokenByAccountDID(ctx, b, "owner"); string(got.EncryptedPayload) != "10" {
		t.Fatalf("token = %q", got.EncryptedPayload)
	}
	if got, _ := s.SyncTargets(ctx, b); !slices.Equal(got, []string{"10"}) {
		t.Fatalf("targets = %v", got)
	}
	if got, _ := s.SourceOperationBySourceURI(ctx, SourceRef{Scope: b, URI: "same"}); got != "10" {
		t.Fatalf("operation = %q", got)
	}
	if got, _ := s.EventMappingBySourceURI(ctx, SourceRef{Scope: b, URI: "same"}); got.NostrEventID != "10" {
		t.Fatalf("mapping = %#v", got)
	}
	if got, _ := s.Cursor(ctx, bOther, "stream"); got != "15" {
		t.Fatalf("other account cursor = %q", got)
	}
	if got, _ := s.EventMappingBySourceURI(ctx, SourceRef{Scope: bOther, URI: "same"}); got.NostrEventID != "15" {
		t.Fatalf("other account mapping = %#v", got)
	}
}
