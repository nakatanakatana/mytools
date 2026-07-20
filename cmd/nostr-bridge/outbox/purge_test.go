package outbox

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

var purgeTestScope = store.SourceScope{Provider: "bluesky", Account: "owner"}

func purgeTestRef(uri string) store.SourceRef {
	return store.SourceRef{Scope: purgeTestScope, URI: uri}
}

func TestPurgeOrdersKindFiveBeforeUnallowAndCleansAuthorOnly(t *testing.T) {
	ctx := context.Background()
	s, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "purge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	key := nostr.Generate()
	pubkey := key.Public().Hex()
	other := nostr.Generate().Public().Hex()
	now := time.Unix(100, 0)
	if err := s.SetPublisherRegistered(ctx, pubkey, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEventMapping(ctx, store.EventMapping{Source: purgeTestRef("owned"), NostrEventID: signedEvent(t).ID.Hex(), AuthorPubKey: pubkey}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEventMapping(ctx, store.EventMapping{Source: purgeTestRef("other"), NostrEventID: signedEvent(t).ID.Hex(), AuthorPubKey: other}); err != nil {
		t.Fatal(err)
	}
	ids, err := s.EventIDsByAuthor(ctx, pubkey)
	if err != nil {
		t.Fatal(err)
	}
	deletion := nostr.Event{CreatedAt: nostr.Now(), Kind: 5, Content: "explicit purge"}
	for _, id := range ids {
		deletion.Tags = append(deletion.Tags, nostr.Tag{"e", id})
	}
	if err := deletion.Sign(key); err != nil {
		t.Fatal(err)
	}
	if err := EnqueuePurge(ctx, s, key.Public(), deletion, 2); err != nil {
		t.Fatal(err)
	}
	dispatchNow := time.Now().Add(time.Second)
	m, p := &managementFake{}, &publisherFake{}
	d := Dispatcher{Store: s, Management: m, Publisher: p, Now: func() time.Time { return dispatchNow }}
	if worked, err := d.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("publish = %v, %v", worked, err)
	}
	if len(m.calls) != 0 {
		t.Fatal("unallow ran before kind5 completion")
	}
	if worked, err := d.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("unallow = %v, %v", worked, err)
	}
	if registered, _ := s.PublisherRegistered(ctx, pubkey); registered {
		t.Fatal("registration retained")
	}
	if _, err := s.EventMappingBySourceURI(ctx, purgeTestRef("owned")); err == nil {
		t.Fatal("owned mapping retained")
	}
	if _, err := s.EventMappingBySourceURI(ctx, purgeTestRef("other")); err != nil {
		t.Fatal("unrelated mapping removed")
	}
	future := nostr.Event{CreatedAt: nostr.Now(), Kind: 1}
	if err := future.Sign(key); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: pubkey, Operation: store.OutboxPublishEvent, PubKey: pubkey, Payload: future.String(), AvailableAt: time.Now()}); !errors.Is(err, store.ErrPurgePending) {
		t.Fatalf("post-completion enqueue = %v", err)
	}
}

func TestPurgeValidatesDeletionAndRollsBackAtLimit(t *testing.T) {
	ctx := context.Background()
	s, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	key := nostr.Generate()
	_ = s.SetPublisherRegistered(ctx, key.Public().Hex(), time.Now())
	bad := nostr.Event{Kind: 1}
	if err := bad.Sign(key); err != nil {
		t.Fatal(err)
	}
	if err := EnqueuePurge(ctx, s, key.Public(), bad, 2); err == nil {
		t.Fatal("accepted non-kind5")
	}
	deletion := nostr.Event{CreatedAt: nostr.Now(), Kind: 5}
	if err := deletion.Sign(key); err != nil {
		t.Fatal(err)
	}
	filler := nostr.Event{CreatedAt: nostr.Now(), Kind: 1}
	if err := filler.Sign(key); err != nil {
		t.Fatal(err)
	}
	_ = s.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: key.Public().Hex(), Operation: store.OutboxPublishEvent, PubKey: key.Public().Hex(), Payload: filler.String(), AvailableAt: time.Now()})
	if err := EnqueuePurge(ctx, s, key.Public(), deletion, 2); !errors.Is(err, store.ErrOutboxFull) {
		t.Fatalf("EnqueuePurge = %v", err)
	}
	if count, _ := s.OutboxCount(ctx); count != 1 {
		t.Fatalf("outbox count = %d", count)
	}
}

func TestPurgeFailureBlocksUnallowAndRepeatedEnqueueIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	key := nostr.Generate()
	_ = s.SetPublisherRegistered(ctx, key.Public().Hex(), time.Now())
	deletion := nostr.Event{CreatedAt: nostr.Now(), Kind: 5}
	if err := deletion.Sign(key); err != nil {
		t.Fatal(err)
	}
	if err := EnqueuePurge(ctx, s, key.Public(), deletion, 4); err != nil {
		t.Fatal(err)
	}
	if err := EnqueuePurge(ctx, s, key.Public(), deletion, 4); err != nil {
		t.Fatal(err)
	}
	if count, _ := s.OutboxCount(ctx); count != 2 {
		t.Fatalf("idempotent count = %d", count)
	}
	now := time.Now().Add(time.Second)
	management := &managementFake{}
	d := Dispatcher{Store: s, Management: management, Publisher: &publisherFake{err: errors.New("relay down")}, Now: func() time.Time { return now }}
	if worked, err := d.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("dispatch = %v, %v", worked, err)
	}
	if len(management.calls) != 0 {
		t.Fatal("unallow delivered after failed kind5")
	}
	restarted := Dispatcher{Store: s, Management: management, Publisher: &publisherFake{}, Now: func() time.Time { return now }}
	if worked, err := restarted.DispatchOne(ctx); err != nil || worked {
		t.Fatalf("restart before backoff = %v, %v", worked, err)
	}
}

func TestPurgeMarkerRejectsFutureDeliveryAndDifferentPurge(t *testing.T) {
	ctx := context.Background()
	s, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "marker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	key := nostr.Generate()
	pubkey := key.Public().Hex()
	_ = s.SetPublisherRegistered(ctx, pubkey, time.Now())
	deletion := nostr.Event{CreatedAt: nostr.Now(), Kind: 5}
	if err := deletion.Sign(key); err != nil {
		t.Fatal(err)
	}
	if err := EnqueuePurge(ctx, s, key.Public(), deletion, 2); err != nil {
		t.Fatal(err)
	}
	other := nostr.Event{CreatedAt: deletion.CreatedAt + 1, Kind: 5}
	if err := other.Sign(key); err != nil {
		t.Fatal(err)
	}
	if err := EnqueuePurge(ctx, s, key.Public(), other, 4); !errors.Is(err, store.ErrPurgePending) {
		t.Fatalf("different purge = %v", err)
	}
	if err := s.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: pubkey, Operation: store.OutboxAllowPublisher, PubKey: pubkey, AvailableAt: time.Now()}); !errors.Is(err, store.ErrPurgePending) {
		t.Fatalf("future allow = %v", err)
	}
	otherKey := nostr.Generate().Public().Hex()
	if err := s.EnqueueEvent(ctx, store.EventEnqueueRequest{Mapping: store.EventMapping{Source: purgeTestRef("smuggle"), AuthorPubKey: pubkey}, Event: store.OutboxRequest{AggregateKey: otherKey, Operation: store.OutboxPublishEvent, PubKey: otherKey, Payload: `{}`, AvailableAt: time.Now()}, Limit: 10}); !errors.Is(err, store.ErrAuthorMismatch) {
		t.Fatalf("cross-author enqueue = %v", err)
	}
	if count, _ := s.OutboxCount(ctx); count != 2 {
		t.Fatalf("cross-author mutation count = %d", count)
	}
}

func TestPurgeDeduplicatesKnownIDsAndRejectsGenericUnallowConflict(t *testing.T) {
	ctx := context.Background()
	s, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "dedupe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	key := nostr.Generate()
	pubkey := key.Public().Hex()
	_ = s.SetPublisherRegistered(ctx, pubkey, time.Now())
	eventID := signedEvent(t).ID.Hex()
	for _, uri := range []string{"one", "two"} {
		if err := s.SaveEventMapping(ctx, store.EventMapping{Source: purgeTestRef(uri), NostrEventID: eventID, AuthorPubKey: pubkey}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: pubkey, Operation: store.OutboxUnallowPublisher, PubKey: pubkey, AvailableAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	deletion := nostr.Event{CreatedAt: nostr.Now(), Kind: 5, Tags: nostr.Tags{{"e", eventID}}}
	if err := deletion.Sign(key); err != nil {
		t.Fatal(err)
	}
	if err := EnqueuePurge(ctx, s, key.Public(), deletion, 3); !errors.Is(err, store.ErrPurgeConflict) {
		t.Fatalf("EnqueuePurge = %v", err)
	}
	if count, _ := s.OutboxCount(ctx); count != 1 {
		t.Fatalf("count = %d, want unchanged generic unallow", count)
	}
	if registered, _ := s.PublisherRegistered(ctx, pubkey); !registered {
		t.Fatal("registration changed")
	}
	if _, err := s.EventMappingBySourceURI(ctx, purgeTestRef("one")); err != nil {
		t.Fatal("mapping changed")
	}
}

func TestPurgeAcceptsOneTagForDuplicateKnownEventID(t *testing.T) {
	ctx := context.Background()
	s, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "distinct.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	key := nostr.Generate()
	pubkey := key.Public().Hex()
	_ = s.SetPublisherRegistered(ctx, pubkey, time.Now())
	eventID := signedEvent(t).ID.Hex()
	for _, uri := range []string{"one", "two"} {
		if err := s.SaveEventMapping(ctx, store.EventMapping{Source: purgeTestRef(uri), NostrEventID: eventID, AuthorPubKey: pubkey}); err != nil {
			t.Fatal(err)
		}
	}
	deletion := nostr.Event{CreatedAt: nostr.Now(), Kind: 5, Tags: nostr.Tags{{"e", eventID}}}
	if err := deletion.Sign(key); err != nil {
		t.Fatal(err)
	}
	if err := EnqueuePurge(ctx, s, key.Public(), deletion, 2); err != nil {
		t.Fatal(err)
	}
}

func TestGenericQueueCannotSpoofAuthorOrCrossAggregateUnallow(t *testing.T) {
	ctx := context.Background()
	s, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "generic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	a, b := nostr.Generate(), nostr.Generate()
	_ = s.SetPublisherRegistered(ctx, a.Public().Hex(), time.Now())
	deletion := nostr.Event{CreatedAt: nostr.Now(), Kind: 5}
	if err := deletion.Sign(a); err != nil {
		t.Fatal(err)
	}
	if err := EnqueuePurge(ctx, s, a.Public(), deletion, 2); err != nil {
		t.Fatal(err)
	}
	spoofed := nostr.Event{CreatedAt: nostr.Now(), Kind: 1}
	if err := spoofed.Sign(a); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: b.Public().Hex(), Operation: store.OutboxPublishEvent, PubKey: b.Public().Hex(), Payload: spoofed.String(), AvailableAt: time.Now()}); !errors.Is(err, store.ErrAuthorMismatch) {
		t.Fatalf("spoof = %v", err)
	}
	if count, _ := s.OutboxCount(ctx); count != 2 {
		t.Fatalf("spoof changed count = %d", count)
	}
	path := filepath.Join(t.TempDir(), "cross-unallow.db")
	clean, closeClean, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeClean.Close() }()
	_ = clean.SetPublisherRegistered(ctx, a.Public().Hex(), time.Now())
	if err := clean.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: b.Public().Hex(), Operation: store.OutboxUnallowPublisher, PubKey: a.Public().Hex(), AvailableAt: time.Now()}); !errors.Is(err, store.ErrAuthorMismatch) {
		t.Fatalf("cross unallow = %v", err)
	}
	if err := EnqueuePurge(ctx, clean, a.Public(), deletion, 2); err != nil {
		t.Fatalf("purge after rejected cross unallow = %v", err)
	}
}

func TestConcurrentPurgeAndEventNeverQueueEventAfterUnallow(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent.db")
	first, closeFirst, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeFirst.Close() }()
	second, closeSecond, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeSecond.Close() }()
	key := nostr.Generate()
	pubkey := key.Public().Hex()
	_ = first.SetPublisherRegistered(ctx, pubkey, time.Now())
	deletion := nostr.Event{CreatedAt: nostr.Now(), Kind: 5}
	if err := deletion.Sign(key); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- EnqueuePurge(ctx, first, key.Public(), deletion, 3) }()
	event := nostr.Event{CreatedAt: nostr.Now(), Kind: 1}
	if err := event.Sign(key); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-start
		results <- second.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: pubkey, Operation: store.OutboxPublishEvent, PubKey: pubkey, Payload: event.String(), AvailableAt: time.Now()})
	}()
	close(start)
	a, b := <-results, <-results
	for _, err := range []error{a, b} {
		if err != nil && !errors.Is(err, store.ErrPurgePending) && !strings.Contains(err.Error(), "database is locked") {
			t.Fatalf("concurrent result = %v", err)
		}
	}
	var operations []store.OutboxOperation
	for {
		items, err := first.ClaimOutbox(ctx, time.Now().Add(time.Second), time.Minute, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		operations = append(operations, items[0].Operation)
		if err := first.CompleteOutbox(ctx, items[0].ID, items[0].ClaimToken, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for i, operation := range operations {
		if operation == store.OutboxUnallowPublisher && i+1 < len(operations) {
			t.Fatalf("row queued after unallow: %#v", operations)
		}
	}
}
