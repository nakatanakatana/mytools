package outbox

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/relayclient"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

type managementFake struct {
	calls []string
	err   error
}

func (f *managementFake) AllowPubKey(_ context.Context, _ nostr.PubKey, _ string) error {
	f.calls = append(f.calls, "allow")
	return f.err
}
func (f *managementFake) UnallowPubKey(_ context.Context, _ nostr.PubKey, _ string) error {
	f.calls = append(f.calls, "unallow")
	return f.err
}

type publisherFake struct {
	calls int
	err   error
}
type claimLostStore struct {
	store.SQLiteStore
	retries int
}

func (s *claimLostStore) CompleteOutbox(context.Context, int64, string, time.Time) error {
	return store.ErrClaimLost
}
func (s *claimLostStore) RetryOutbox(ctx context.Context, id int64, token string, now, available time.Time, message string) error {
	s.retries++
	return s.SQLiteStore.RetryOutbox(ctx, id, token, now, available, message)
}

func (f *publisherFake) Publish(context.Context, nostr.Event) error { f.calls++; return f.err }

type blockingPublisher struct{}

func (blockingPublisher) Publish(ctx context.Context, _ nostr.Event) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestDispatcherBoundsExternalDeliveryAttempt(t *testing.T) {
	ctx := context.Background()
	s, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "timeout.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	e := signedEvent(t)
	now := time.Now()
	if err := s.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: e.PubKey.Hex(), Operation: store.OutboxPublishEvent, PubKey: e.PubKey.Hex(), Payload: e.String(), AvailableAt: now}); err != nil {
		t.Fatal(err)
	}
	d := Dispatcher{Store: s, Publisher: blockingPublisher{}, DeliveryTimeout: 20 * time.Millisecond, Now: func() time.Time { return now }}
	started := time.Now()
	worked, err := d.DispatchOne(ctx)
	if err != nil || !worked {
		t.Fatalf("DispatchOne() = %v, %v", worked, err)
	}
	if time.Since(started) > 250*time.Millisecond {
		t.Fatalf("delivery was not bounded: %s", time.Since(started))
	}
}

func TestDispatcherOrdersAllowBeforePublishAndRegisters(t *testing.T) {
	ctx := context.Background()
	s, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	e := signedEvent(t)
	now := time.Unix(100, 0)
	if err := s.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: e.PubKey.Hex(), Operation: store.OutboxAllowPublisher, PubKey: e.PubKey.Hex(), AvailableAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: e.PubKey.Hex(), Operation: store.OutboxPublishEvent, PubKey: e.PubKey.Hex(), Payload: e.String(), AvailableAt: now}); err != nil {
		t.Fatal(err)
	}
	m, p := &managementFake{}, &publisherFake{}
	d := Dispatcher{Store: s, Management: m, Publisher: p, Now: func() time.Time { return now }}
	if worked, err := d.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("first = %v, %v", worked, err)
	}
	if registered, _ := s.PublisherRegistered(ctx, e.PubKey.Hex()); !registered {
		t.Fatal("publisher was not registered")
	}
	if p.calls != 0 {
		t.Fatal("event published before allow completion")
	}
	if worked, err := d.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("second = %v, %v", worked, err)
	}
	if p.calls != 1 {
		t.Fatalf("publish calls = %d", p.calls)
	}
	if count, _ := s.OutboxCount(ctx); count != 0 {
		t.Fatalf("outbox count = %d", count)
	}
}

func TestDispatcherRetriesFailureAndBlocksAggregate(t *testing.T) {
	ctx := context.Background()
	s, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	e := signedEvent(t)
	now := time.Unix(100, 0)
	_ = s.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: e.PubKey.Hex(), Operation: store.OutboxAllowPublisher, PubKey: e.PubKey.Hex(), AvailableAt: now})
	_ = s.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: e.PubKey.Hex(), Operation: store.OutboxPublishEvent, PubKey: e.PubKey.Hex(), Payload: e.String(), AvailableAt: now})
	p := &publisherFake{}
	d := Dispatcher{Store: s, Management: &managementFake{err: errors.New("secret\n" + string(make([]byte, 1000)))}, Publisher: p, BaseBackoff: time.Second, MaxBackoff: time.Minute, Now: func() time.Time { return now }}
	if worked, err := d.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("dispatch = %v, %v", worked, err)
	}
	if p.calls != 0 {
		t.Fatal("event published after failed allow")
	}
	items, err := s.ClaimOutbox(ctx, now.Add(time.Second), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim retry = %d, %v", len(items), err)
	}
	if items[0].Attempts != 1 || len(items[0].LastError) > 256 {
		t.Fatalf("retry metadata = %#v", items[0])
	}
}

func TestDispatcherRecoversRelayPublisherAllowlistDrift(t *testing.T) {
	ctx := context.Background()
	s, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "drift.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	e := signedEvent(t)
	now := time.Unix(100, 0)
	if err := s.SetPublisherRegistered(ctx, e.PubKey.Hex(), now); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: e.PubKey.Hex(), Operation: store.OutboxPublishEvent, PubKey: e.PubKey.Hex(), Payload: e.String(), AvailableAt: now}); err != nil {
		t.Fatal(err)
	}
	p := &publisherFake{err: &relayclient.PublisherNotAllowedError{Reason: "restricted: publisher not allowed"}}
	m := &managementFake{}
	d := Dispatcher{Store: s, Management: m, Publisher: p, Now: func() time.Time { return now }}
	if worked, err := d.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("rejected publish = %v, %v", worked, err)
	}
	if registered, _ := s.PublisherRegistered(ctx, e.PubKey.Hex()); registered {
		t.Fatal("stale registration remained")
	}
	p.err = nil
	if worked, err := d.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("allow = %v, %v", worked, err)
	}
	if len(m.calls) != 1 || m.calls[0] != "allow" {
		t.Fatalf("management calls = %#v", m.calls)
	}
	if worked, err := d.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("retry publish = %v, %v", worked, err)
	}
	if p.calls != 2 {
		t.Fatalf("publish calls = %d", p.calls)
	}
	if count, _ := s.OutboxCount(ctx); count != 0 {
		t.Fatalf("outbox count = %d", count)
	}
}

func TestDispatcherUnallowClearsRegistrationAtomically(t *testing.T) {
	ctx := context.Background()
	s, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "unallow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	e := signedEvent(t)
	now := time.Unix(100, 0)
	_ = s.SetPublisherRegistered(ctx, e.PubKey.Hex(), now)
	if err := s.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: e.PubKey.Hex(), Operation: store.OutboxUnallowPublisher, PubKey: e.PubKey.Hex(), AvailableAt: now}); err != nil {
		t.Fatal(err)
	}
	d := Dispatcher{Store: s, Management: &managementFake{}, Publisher: &publisherFake{}, Now: func() time.Time { return now }}
	if worked, err := d.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("dispatch = %v, %v", worked, err)
	}
	if registered, _ := s.PublisherRegistered(ctx, e.PubKey.Hex()); registered {
		t.Fatal("registration remained after unallow")
	}
}

func TestDispatcherRunReturnsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, closer, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	err = (&Dispatcher{Store: s}).Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v", err)
	}
}

func TestGenericOutboxRejectsMalformedPayloadBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	s, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "restart.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	e := signedEvent(t)
	now := time.Unix(100, 0)
	_ = s.SetPublisherRegistered(ctx, e.PubKey.Hex(), now)
	err = s.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: e.PubKey.Hex(), Operation: store.OutboxPublishEvent, PubKey: e.PubKey.Hex(), Payload: `{broken`, AvailableAt: now})
	if !errors.Is(err, store.ErrInvalidOutboxPayload) {
		t.Fatalf("enqueue = %v", err)
	}
	if count, _ := s.OutboxCount(ctx); count != 0 {
		t.Fatalf("outbox count = %d", count)
	}
}

func TestDispatcherClaimLostDoesNotRetryWithStaleToken(t *testing.T) {
	ctx := context.Background()
	sqlite, closer, err := store.Open(ctx, filepath.Join(t.TempDir(), "lost.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	e := signedEvent(t)
	now := time.Unix(100, 0)
	_ = sqlite.SetPublisherRegistered(ctx, e.PubKey.Hex(), now)
	_ = sqlite.EnqueueOutbox(ctx, store.OutboxRequest{AggregateKey: e.PubKey.Hex(), Operation: store.OutboxPublishEvent, PubKey: e.PubKey.Hex(), Payload: e.String(), AvailableAt: now})
	wrapper := &claimLostStore{SQLiteStore: sqlite}
	d := Dispatcher{Store: wrapper, Publisher: &publisherFake{}, Now: func() time.Time { return now }}
	if worked, err := d.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("dispatch = %v, %v", worked, err)
	}
	if wrapper.retries != 0 {
		t.Fatalf("retry calls = %d", wrapper.retries)
	}
	if count, _ := sqlite.OutboxCount(ctx); count != 1 {
		t.Fatalf("row count = %d", count)
	}
}

func TestDispatcherBackoffCapsAtMaximum(t *testing.T) {
	d := Dispatcher{BaseBackoff: time.Second, MaxBackoff: 8 * time.Second}
	if got := d.backoff(100); got != 8*time.Second {
		t.Fatalf("backoff = %v", got)
	}
}

func signedEvent(t *testing.T) nostr.Event {
	t.Helper()
	e := nostr.Event{CreatedAt: nostr.Now(), Kind: 1, Content: "outbox"}
	if err := e.Sign(nostr.Generate()); err != nil {
		t.Fatal(err)
	}
	return e
}
