package main

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/fasthttp/websocket"
	bridgeoutbox "github.com/nakatanakatana/mytools/cmd/nostr-bridge/outbox"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/relayclient"
	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

func TestRelayBridgeProtocolIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	adminSK := nostr.Generate()
	readerSK := nostr.Generate()
	publisherSK := nostr.Generate()
	publisher := nostr.GetPublicKey(publisherSK)
	databasePath := t.TempDir() + "/relay.db"

	first := startBridgeProtocolRelay(t, databasePath, adminSK, nostr.GetPublicKey(readerSK))
	management := bridgeManagementClient(t, first, adminSK)
	if err := management.AllowPubKey(ctx, publisher, "bridge integration test"); err != nil {
		t.Fatalf("allow publisher through NIP-86: %v", err)
	}

	publisherClient := &relayclient.WebSocketPublisher{RelayURL: first.websocketURL}
	original := signedBridgeProtocolEvent(t, publisherSK, 1, nil, "persists across relay restart")
	if err := publisherClient.Publish(ctx, original); err != nil {
		t.Fatalf("publish through WebSocket EVENT: %v", err)
	}
	assertBridgeReaderEvents(t, first.websocketURL, readerSK, original.ID, original.ID)
	first.close(t)

	second := startBridgeProtocolRelay(t, databasePath, adminSK, nostr.GetPublicKey(readerSK))
	assertBridgeReaderEvents(t, second.websocketURL, readerSK, original.ID, original.ID)

	// The allowlist must survive independently of the event store. Publishing a
	// new event after restart proves the bridge does not need to call NIP-86 again.
	publisherClient = &relayclient.WebSocketPublisher{RelayURL: second.websocketURL}
	afterRestart := signedBridgeProtocolEvent(t, publisherSK, 1, nil, "publisher remains allowed")
	if err := publisherClient.Publish(ctx, afterRestart); err != nil {
		t.Fatalf("publish with persisted publisher allowlist: %v", err)
	}
	assertBridgeReaderEvents(t, second.websocketURL, readerSK, afterRestart.ID, afterRestart.ID)

	bridgeStore, bridgeStoreCloser, err := bridgestore.Open(ctx, filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bridgeStoreCloser.Close() }()
	if err := bridgeStore.SetPublisherRegistered(ctx, publisher.Hex(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := bridgeStore.SaveEventMapping(ctx, bridgestore.EventMapping{SourceURI: "at://bridge/original", NostrEventID: original.ID.Hex(), AuthorPubKey: publisher.Hex()}); err != nil {
		t.Fatal(err)
	}
	deletion := signedBridgeProtocolEvent(t, publisherSK, 5, nostr.Tags{{"e", original.ID.Hex()}}, "bridge purge")
	if err := bridgeoutbox.EnqueuePurge(ctx, bridgeStore, publisher, deletion, 10); err != nil {
		t.Fatal(err)
	}
	dispatcher := bridgeoutbox.Dispatcher{
		Store:      bridgeStore,
		Management: bridgeManagementClient(t, second, adminSK),
		Publisher:  publisherClient,
		Now:        func() time.Time { return time.Now().Add(time.Second) },
	}
	if worked, err := dispatcher.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("dispatch kind 5 = %v, %v", worked, err)
	}
	assertBridgeReaderEvents(t, second.websocketURL, readerSK, original.ID)
	assertBridgeReaderEvents(t, second.websocketURL, readerSK, deletion.ID, deletion.ID)
	beforeUnallow := signedBridgeProtocolEvent(t, publisherSK, 1, nil, "allowed until kind 5 completes")
	if err := publisherClient.Publish(ctx, beforeUnallow); err != nil {
		t.Fatalf("publisher was unallowed before kind 5 completion: %v", err)
	}
	if worked, err := dispatcher.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("dispatch unallow = %v, %v", worked, err)
	}
	rejected := signedBridgeProtocolEvent(t, publisherSK, 1, nil, "must be rejected after unallow")
	if err := publisherClient.Publish(ctx, rejected); err == nil {
		t.Fatal("publish after NIP-86 unallow succeeded")
	}
}

func TestRelayBridgePurgePublishFailureDoesNotUnallow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	adminSK := nostr.Generate()
	readerSK := nostr.Generate()
	publisherSK := nostr.Generate()
	publisher := nostr.GetPublicKey(publisherSK)
	relay := startBridgeProtocolRelay(t, t.TempDir()+"/relay.db", adminSK, nostr.GetPublicKey(readerSK))
	management := bridgeManagementClient(t, relay, adminSK)
	if err := management.AllowPubKey(ctx, publisher, "failed purge test"); err != nil {
		t.Fatal(err)
	}

	original := signedBridgeProtocolEvent(t, publisherSK, 1, nil, "retained after failed purge")
	realPublisher := &relayclient.WebSocketPublisher{RelayURL: relay.websocketURL}
	if err := realPublisher.Publish(ctx, original); err != nil {
		t.Fatal(err)
	}
	bridgeStore, closer, err := bridgestore.Open(ctx, filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close() }()
	if err := bridgeStore.SetPublisherRegistered(ctx, publisher.Hex(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := bridgeStore.SaveEventMapping(ctx, bridgestore.EventMapping{SourceURI: "at://bridge/failure", NostrEventID: original.ID.Hex(), AuthorPubKey: publisher.Hex()}); err != nil {
		t.Fatal(err)
	}
	deletion := signedBridgeProtocolEvent(t, publisherSK, 5, nostr.Tags{{"e", original.ID.Hex()}}, "failed purge")
	if err := bridgeoutbox.EnqueuePurge(ctx, bridgeStore, publisher, deletion, 10); err != nil {
		t.Fatal(err)
	}
	dispatcher := bridgeoutbox.Dispatcher{
		Store:      bridgeStore,
		Management: management,
		Publisher: &relayclient.WebSocketPublisher{
			RelayURL: relay.websocketURL,
			Dial: func(context.Context, string) (relayclient.Conn, error) {
				return nil, errors.New("injected connection failure")
			},
		},
		Now: func() time.Time { return time.Now().Add(time.Second) },
	}
	if worked, err := dispatcher.DispatchOne(ctx); err != nil || !worked {
		t.Fatalf("failed kind 5 dispatch = %v, %v", worked, err)
	}
	stillAllowed := signedBridgeProtocolEvent(t, publisherSK, 1, nil, "publisher remains allowed")
	if err := realPublisher.Publish(ctx, stillAllowed); err != nil {
		t.Fatalf("failed kind 5 unallowed publisher: %v", err)
	}
	assertBridgeReaderEvents(t, relay.websocketURL, readerSK, original.ID, original.ID)
}

type bridgeProtocolRelay struct {
	resources    RelayResources
	server       *httptest.Server
	httpURL      *url.URL
	websocketURL string
	closed       bool
}

func startBridgeProtocolRelay(t *testing.T, databasePath string, adminSK nostr.SecretKey, reader nostr.PubKey) *bridgeProtocolRelay {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serviceURL := "http://" + listener.Addr().String()
	resources, err := NewRelay(context.Background(), Config{
		Mode:          ModePrivateSQLite,
		DatabasePath:  databasePath,
		ServiceURL:    serviceURL,
		AdminPubkey:   nostr.GetPublicKey(adminSK).Hex(),
		ReaderPubkeys: []string{reader.Hex()},
		MaxQueryLimit: 10,
	})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(resources.Handler)
	server.Listener = listener
	server.Start()
	parsedURL, err := url.Parse(server.URL)
	if err != nil {
		server.Close()
		_ = resources.Close()
		t.Fatal(err)
	}
	running := &bridgeProtocolRelay{
		resources:    resources,
		server:       server,
		httpURL:      parsedURL,
		websocketURL: "ws" + strings.TrimPrefix(server.URL, "http"),
	}
	t.Cleanup(func() {
		if !running.closed {
			running.close(t)
		}
	})
	return running
}

func (r *bridgeProtocolRelay) close(t *testing.T) {
	t.Helper()
	if r.closed {
		return
	}
	r.closed = true
	r.server.Close()
	if err := r.resources.Close(); err != nil {
		t.Error(err)
	}
}

func bridgeManagementClient(t *testing.T, relay *bridgeProtocolRelay, adminSK nostr.SecretKey) *relayclient.HTTPManagementClient {
	t.Helper()
	client, err := relayclient.NewHTTPManagementClient(relay.httpURL, relay.httpURL, adminSK)
	if err != nil {
		t.Fatal(err)
	}
	client.HTTPClient = relay.server.Client()
	return client
}

func signedBridgeProtocolEvent(t *testing.T, sk nostr.SecretKey, kind nostr.Kind, tags nostr.Tags, content string) nostr.Event {
	t.Helper()
	event := nostr.Event{CreatedAt: nostr.Now(), Kind: kind, Tags: tags, Content: content}
	if err := event.Sign(sk); err != nil {
		t.Fatal(err)
	}
	return event
}

func assertBridgeReaderEvents(t *testing.T, relayURL string, readerSK nostr.SecretKey, queryID nostr.ID, wantIDs ...nostr.ID) {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(relayURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	subscriptionID := "bridge-integration-" + queryID.Hex()[:8]
	challenge := requestAuthChallenge(t, conn, subscriptionID+"-auth")
	authenticateForURL(t, conn, readerSK, challenge, relayURL)
	if err := conn.WriteJSON([]any{"REQ", subscriptionID, map[string]any{"ids": []string{queryID.Hex()}}}); err != nil {
		t.Fatal(err)
	}

	want := make(map[nostr.ID]struct{}, len(wantIDs))
	for _, id := range wantIDs {
		want[id] = struct{}{}
	}
	got := make(map[nostr.ID]struct{}, len(wantIDs))
	for {
		message := readWSMessage(t, conn)
		switch labelOf(t, message) {
		case "EVENT":
			var event nostr.Event
			if err := event.UnmarshalJSON(message[2]); err != nil {
				t.Fatal(err)
			}
			got[event.ID] = struct{}{}
		case "EOSE":
			if len(got) != len(want) {
				t.Fatalf("queried event IDs = %v, want %v", got, want)
			}
			for id := range want {
				if _, ok := got[id]; !ok {
					t.Fatalf("queried event IDs = %v, missing %s", got, id)
				}
			}
			return
		default:
			t.Fatalf("unexpected reader response: %s", message)
		}
	}
}
