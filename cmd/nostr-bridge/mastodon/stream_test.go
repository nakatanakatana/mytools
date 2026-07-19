package mastodon

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

type fakeStreamConn struct {
	writes [][]byte
	reads  [][]byte
	err    error
}

func (f *fakeStreamConn) Write(_ context.Context, b []byte) error {
	f.writes = append(f.writes, append([]byte(nil), b...))
	return nil
}
func (f *fakeStreamConn) Read(context.Context) ([]byte, error) {
	if len(f.reads) == 0 {
		return nil, f.err
	}
	b := f.reads[0]
	f.reads = f.reads[1:]
	return b, nil
}
func (f *fakeStreamConn) Close() error { return nil }

func TestStreamRestoresUserAndListSubscriptions(t *testing.T) {
	c := &fakeStreamConn{}
	if err := subscribe(context.Background(), c, []string{"7", "9", "7"}); err != nil {
		t.Fatal(err)
	}
	var got []subscription
	for _, b := range c.writes {
		var v subscription
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	want := []subscription{{Type: "subscribe", Stream: "user"}, {Type: "subscribe", Stream: "list", List: "7"}, {Type: "subscribe", Stream: "list", List: "9"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subscriptions=%#v", got)
	}
}

func TestDecodeStreamEventRejectsMalformedAndIgnoresUnknown(t *testing.T) {
	if _, err := decodeStreamEvent([]byte(`{"event":"update","payload":"{"}`)); err == nil {
		t.Fatal("expected malformed payload error")
	}
	e, err := decodeStreamEvent([]byte(`{"event":"filters_changed","payload":"x"}`))
	if err != nil || e.Event != "filters_changed" {
		t.Fatalf("event=%+v err=%v", e, err)
	}
}

func TestRunReconnectsAndRestoresSubscriptions(t *testing.T) {
	api := &fakeTimelineAPI{lists: map[string][]Status{}}
	store := newMemoryDelivery()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connections := []*fakeStreamConn{{err: errors.New("drop")}, {err: context.Canceled}}
	i := 0
	s := NewSyncer(SyncOptions{Scope: testScope(), API: api, Store: store, MasterSeed: []byte(testSeed), ListIDs: []string{"7", "7"}, StreamURL: "wss://example", Connect: func(context.Context, string) (StreamConnection, error) {
		c := connections[i]
		i++
		if i == 2 {
			cancel()
		}
		return c, nil
	}, Sleep: func(context.Context, int) error { return nil }})
	_ = s.Run(ctx)
	if i != 2 {
		t.Fatalf("connections=%d", i)
	}
	for n, c := range connections {
		if len(c.writes) != 2 {
			t.Fatalf("connection %d subscriptions=%d", n, len(c.writes))
		}
	}
}

func TestBackoffCapsAndCancels(t *testing.T) {
	if got := backoffDuration(100); got != 64*time.Second {
		t.Fatalf("backoff=%s", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := backoffSleep(ctx, 6); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation delayed")
	}
}

func testScope() store.SourceScope { return store.SourceScope{Provider: "mastodon", Account: "owner"} }

func TestRunPerformsRESTGapRecoveryBeforeLiveProcessing(t *testing.T) {
	status := testStatus("42", "https://social.example/users/alice/statuses/42", "https://social.example/users/alice")
	api := &fakeTimelineAPI{home: []Status{status}, lists: map[string][]Status{}}
	conn := &fakeStreamConn{err: errors.New("closed")}
	calls := 0
	store := newMemoryDelivery()
	ctx, cancel := context.WithCancel(context.Background())
	s := testSyncer(api, store, sourceSet(status.Account.URI))
	s.options.StreamURL = "wss://social.example/api/v1/streaming"
	s.options.Connect = func(context.Context, string) (StreamConnection, error) {
		calls++
		if calls > 1 {
			cancel()
			return nil, ctx.Err()
		}
		return conn, nil
	}
	s.options.Sleep = func(context.Context, int) error { return nil }
	_ = s.Run(ctx)
	if len(api.calls) == 0 || api.calls[0] != "home" {
		t.Fatalf("calls=%v", api.calls)
	}
	if len(store.payloads) != 1 {
		t.Fatalf("published=%d", len(store.payloads))
	}
}

func sourceSet(uri string) map[source.ActorIdentity]struct{} {
	return map[source.ActorIdentity]struct{}{{Provider: "mastodon", ID: uri}: {}}
}
