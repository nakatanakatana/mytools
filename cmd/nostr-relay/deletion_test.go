package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/fasthttp/websocket"
	relaystore "github.com/nakatanakatana/mytools/cmd/nostr-relay/store"
)

func TestDeletionProtocol(t *testing.T) {
	t.Run("owned only", func(t *testing.T) {
		ownerSK := nostr.Generate()
		store, conn := deletionProtocolRelay(t, nostr.GetPublicKey(ownerSK))
		target := signedDeletionTestEvent(t, ownerSK, 1, nil, "owned")
		publishDeletionTestEvent(t, conn, target, true)
		deletion := signedDeletionTestEvent(t, ownerSK, 5, nostr.Tags{{"e", target.ID.Hex()}}, "delete")
		publishDeletionTestEvent(t, conn, deletion, true)
		assertDeletionEventMissing(t, store, target.ID)
		assertDeletionEventStored(t, store, deletion.ID)
	})

	t.Run("owned and foreign", func(t *testing.T) {
		ownerSK := nostr.Generate()
		foreignSK := nostr.Generate()
		store, conn := deletionProtocolRelay(t, nostr.GetPublicKey(ownerSK), nostr.GetPublicKey(foreignSK))
		owned := signedDeletionTestEvent(t, ownerSK, 1, nil, "owned")
		foreign := signedDeletionTestEvent(t, foreignSK, 1, nil, "foreign")
		publishDeletionTestEvent(t, conn, owned, true)
		publishDeletionTestEvent(t, conn, foreign, true)
		deletion := signedDeletionTestEvent(t, ownerSK, 5, nostr.Tags{{"e", owned.ID.Hex()}, {"e", foreign.ID.Hex()}}, "mixed")
		publishDeletionTestEvent(t, conn, deletion, true)
		assertDeletionEventMissing(t, store, owned.ID)
		assertDeletionEventStored(t, store, foreign.ID)
		assertDeletionEventStored(t, store, deletion.ID)
	})

	t.Run("malformed and missing", func(t *testing.T) {
		ownerSK := nostr.Generate()
		store, conn := deletionProtocolRelay(t, nostr.GetPublicKey(ownerSK))
		missing := nostr.ID{1}
		deletion := signedDeletionTestEvent(t, ownerSK, 5, nostr.Tags{{"e", "not-an-id"}, {"e", missing.Hex()}}, "ignore invalid")
		publishDeletionTestEvent(t, conn, deletion, true)
		assertDeletionEventStored(t, store, deletion.ID)
	})

	t.Run("replay", func(t *testing.T) {
		ownerSK := nostr.Generate()
		store, conn := deletionProtocolRelay(t, nostr.GetPublicKey(ownerSK))
		target := signedDeletionTestEvent(t, ownerSK, 1, nil, "target")
		publishDeletionTestEvent(t, conn, target, true)
		deletion := signedDeletionTestEvent(t, ownerSK, 5, nostr.Tags{{"e", target.ID.Hex()}}, "delete")
		publishDeletionTestEvent(t, conn, deletion, true)
		publishDeletionTestEvent(t, conn, deletion, true)
		assertDeletionEventMissing(t, store, target.ID)
	})

	t.Run("a tag does not delete", func(t *testing.T) {
		ownerSK := nostr.Generate()
		owner := nostr.GetPublicKey(ownerSK)
		store, conn := deletionProtocolRelay(t, owner)
		target := signedDeletionTestEvent(t, ownerSK, 30000, nostr.Tags{{"d", "keep"}}, "addressable")
		publishDeletionTestEvent(t, conn, target, true)
		deletion := signedDeletionTestEvent(t, ownerSK, 5, nostr.Tags{{"a", "30000:" + owner.Hex() + ":keep"}}, "do not broaden")
		publishDeletionTestEvent(t, conn, deletion, true)
		assertDeletionEventStored(t, store, target.ID)
		assertDeletionEventStored(t, store, deletion.ID)
	})
}

func TestDeletionProtocolDeleteFailureRejected(t *testing.T) {
	ownerSK := nostr.Generate()
	target := signedDeletionTestEvent(t, ownerSK, 1, nil, "target")
	store := &deletionFailureStore{target: target}
	relay := configuredRelay(Config{}, nil)
	relay.OnEvent = func(context.Context, nostr.Event) (bool, string) { return false, "" }
	relay.StoreEvent = func(ctx context.Context, event nostr.Event) error {
		return persistEventAndApplyDeletion(ctx, store, event)
	}
	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	deletion := signedDeletionTestEvent(t, ownerSK, 5, nostr.Tags{{"e", target.ID.Hex()}}, "delete")
	publishDeletionTestEvent(t, conn, deletion, false)
	if store.saved {
		t.Fatal("deletion request was visible after atomic failure")
	}
}

type deletionFailureStore struct {
	relaystore.Store
	target nostr.Event
	saved  bool
}

func (s *deletionFailureStore) SaveEvent(context.Context, nostr.Event) error {
	s.saved = true
	return nil
}

func (s *deletionFailureStore) SaveEventAndApplyDeletion(context.Context, nostr.Event) error {
	return errors.New("injected delete failure")
}

func (s *deletionFailureStore) Event(context.Context, nostr.ID) (nostr.Event, error) {
	return s.target, nil
}

func (s *deletionFailureStore) DeleteEvent(context.Context, nostr.ID) error {
	return errors.New("injected delete failure")
}

func deletionProtocolRelay(t *testing.T, publishers ...nostr.PubKey) (*relaystore.SQLiteStore, *websocket.Conn) {
	t.Helper()
	databasePath := t.TempDir() + "/relay.db"
	store, err := relaystore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, publisher := range publishers {
		if err := store.AllowPublisher(context.Background(), relaystore.Publisher{PubKey: publisher, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	resources, err := newPrivateSQLiteRelay(Config{Mode: ModePrivateSQLite, DatabasePath: databasePath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resources.Close(); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(resources.Relay)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return store, conn
}

func publishDeletionTestEvent(t *testing.T, conn *websocket.Conn, event nostr.Event, wantAccepted bool) {
	t.Helper()
	if err := conn.WriteJSON(nostr.EventEnvelope{Event: event}); err != nil {
		t.Fatal(err)
	}
	message := readWSMessage(t, conn)
	if labelOf(t, message) != "OK" {
		t.Fatalf("response = %s, want OK", message[0])
	}
	var accepted bool
	if err := json.Unmarshal(message[2], &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted != wantAccepted {
		t.Fatalf("OK accepted = %v, want %v; response = %s", accepted, wantAccepted, message)
	}
}

func signedDeletionTestEvent(t *testing.T, sk nostr.SecretKey, kind nostr.Kind, tags nostr.Tags, content string) nostr.Event {
	t.Helper()
	event := nostr.Event{CreatedAt: nostr.Now(), Kind: kind, Tags: tags, Content: content}
	if err := event.Sign(sk); err != nil {
		t.Fatal(err)
	}
	return event
}

func assertDeletionEventStored(t *testing.T, store *relaystore.SQLiteStore, id nostr.ID) {
	t.Helper()
	if _, err := store.Event(context.Background(), id); err != nil {
		t.Fatalf("Event(%s) = %v, want stored", id, err)
	}
}

func assertDeletionEventMissing(t *testing.T, store *relaystore.SQLiteStore, id nostr.ID) {
	t.Helper()
	if _, err := store.Event(context.Background(), id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Event(%s) error = %v, want sql.ErrNoRows", id, err)
	}
}
