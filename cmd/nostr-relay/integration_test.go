package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip42"
	"github.com/fasthttp/websocket"
	relaystore "github.com/nakatanakatana/mytools/cmd/nostr-relay/store"
)

func TestNIP11InformationDocument(t *testing.T) {
	resources, err := NewRelay(context.Background(), Config{
		Name:          "test relay",
		Description:   "test description",
		MaxQueryLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resources.Close(); err != nil {
			t.Error(err)
		}
	})
	relay := resources.Relay

	server := httptest.NewServer(relay)
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/nostr+json")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d", resp.StatusCode)
	}

	var info struct {
		Name          string `json:"name"`
		Description   string `json:"description"`
		SupportedNIPs []any  `json:"supported_nips"`
		Software      string `json:"software"`
		Version       string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Name != "test relay" {
		t.Fatalf("Name = %q", info.Name)
	}
	if info.Description != "test description" {
		t.Fatalf("Description = %q", info.Description)
	}
	if !containsNIP(info.SupportedNIPs, 1) || !containsNIP(info.SupportedNIPs, 11) {
		t.Fatalf("SupportedNIPs = %v", info.SupportedNIPs)
	}
	if info.Software != softwareURL {
		t.Fatalf("Software = %q", info.Software)
	}
	if info.Version == "" {
		t.Fatal("Version is empty")
	}
}

func TestPrivateUnauthenticatedRequestSendsOneAuthChallenge(t *testing.T) {
	reader := nostr.GetPublicKey(nostr.Generate())
	conn := startPrivateRelay(t, reader, nostr.ZeroPK)
	if err := conn.WriteJSON([]any{"REQ", "private", map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	authCount := 0
	closedCount := 0
	for closedCount == 0 {
		var message []json.RawMessage
		if err := conn.ReadJSON(&message); err != nil {
			t.Fatal(err)
		}
		var label string
		if err := json.Unmarshal(message[0], &label); err != nil {
			t.Fatal(err)
		}
		switch label {
		case "AUTH":
			authCount++
		case "CLOSED":
			closedCount++
		}
	}
	if authCount != 1 || closedCount != 1 {
		t.Fatalf("AUTH = %d, CLOSED = %d; want exactly one each", authCount, closedCount)
	}
}

func TestPrivateNIP42ReaderAuthorization(t *testing.T) {
	readerSK := nostr.Generate()
	reader := nostr.GetPublicKey(readerSK)
	outsiderSK := nostr.Generate()
	outsider := startPrivateRelay(t, reader, nostr.ZeroPK)
	challenge := requestAuthChallenge(t, outsider, "outsider")
	authenticate(t, outsider, outsiderSK, challenge)
	if err := outsider.WriteJSON([]any{"REQ", "outsider-again", map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	message := readWSMessage(t, outsider)
	if labelOf(t, message) != "CLOSED" {
		t.Fatalf("response = %s, want CLOSED", message[0])
	}

	allowed := startPrivateRelay(t, reader, nostr.ZeroPK)
	challenge = requestAuthChallenge(t, allowed, "reader")
	authenticate(t, allowed, readerSK, challenge)
	if err := allowed.WriteJSON([]any{"REQ", "reader-again", map[string]any{"limit": 1}}); err != nil {
		t.Fatal(err)
	}
	if message := readWSMessage(t, allowed); labelOf(t, message) != "EOSE" {
		t.Fatalf("response = %s, want EOSE", message[0])
	}
}

func TestPrivateAllowedPublisherEventPersists(t *testing.T) {
	readerSK := nostr.Generate()
	reader := nostr.GetPublicKey(readerSK)
	publisherSK := nostr.Generate()
	publisher := nostr.GetPublicKey(publisherSK)
	conn := startPrivateRelay(t, reader, publisher)
	event := nostr.Event{CreatedAt: nostr.Now(), Kind: 1, Content: "private persisted"}
	if err := event.Sign(publisherSK); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(nostr.EventEnvelope{Event: event}); err != nil {
		t.Fatal(err)
	}
	assertOK(t, readWSMessage(t, conn))

	challenge := requestAuthChallenge(t, conn, "persisted")
	authenticate(t, conn, readerSK, challenge)
	if err := conn.WriteJSON([]any{"REQ", "persisted-again", map[string]any{"ids": []string{event.ID.Hex()}}}); err != nil {
		t.Fatal(err)
	}
	if message := readWSMessage(t, conn); labelOf(t, message) != "EVENT" {
		t.Fatalf("response = %s, want EVENT", message[0])
	}
}

func TestPrivateRelayRestartPreservesPublisherAndEvent(t *testing.T) {
	readerSK := nostr.Generate()
	reader := nostr.GetPublicKey(readerSK)
	adminSK := nostr.Generate()
	publisherSK := nostr.Generate()
	publisher := nostr.GetPublicKey(publisherSK)
	databasePath := t.TempDir() + "/relay.db"
	cfg := Config{
		Mode: ModePrivateSQLite, DatabasePath: databasePath, ServiceURL: "https://relay.example/relay",
		AdminPubkey: nostr.GetPublicKey(adminSK).Hex(), ReaderPubkeys: []string{reader.Hex()}, MaxQueryLimit: 10,
	}

	first, err := NewRelay(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstClosed := false
	t.Cleanup(func() {
		if !firstClosed {
			firstClosed = true
			if err := first.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	body := []byte(`{"method":"allowpubkey","params":["` + publisher.Hex() + `","restart test"]}`)
	response := managementCall(t, first.Handler, adminSK, time.Now(), cfg.ServiceURL, body)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"result":true`) {
		t.Fatalf("allow publisher response: status=%d body=%s", response.Code, response.Body.String())
	}
	event := nostr.Event{CreatedAt: nostr.Now(), Kind: 1, Content: "survives private relay restart"}
	if err := event.Sign(publisherSK); err != nil {
		t.Fatal(err)
	}
	firstServer, firstConn := serveRelay(t, first.Handler)
	if err := firstConn.WriteJSON(nostr.EventEnvelope{Event: event}); err != nil {
		t.Fatal(err)
	}
	assertOK(t, readWSMessage(t, firstConn))
	if err := firstConn.Close(); err != nil {
		t.Fatal(err)
	}
	firstServer.Close()
	firstClosed = true
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := NewRelay(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	})
	_, conn := serveRelay(t, second.Handler)
	challenge := requestAuthChallenge(t, conn, "after-restart")
	authenticateForURL(t, conn, readerSK, challenge, "wss://relay.example/relay")
	if err := conn.WriteJSON([]any{"REQ", "persisted", map[string]any{"ids": []string{event.ID.Hex()}}}); err != nil {
		t.Fatal(err)
	}
	if message := readWSMessage(t, conn); labelOf(t, message) != "EVENT" {
		t.Fatalf("response = %s, want persisted EVENT", message[0])
	}
	if message := readWSMessage(t, conn); labelOf(t, message) != "EOSE" {
		t.Fatalf("response = %s, want EOSE", message[0])
	}

	secondEvent := nostr.Event{CreatedAt: nostr.Now(), Kind: 1, Content: "publisher remains allowed"}
	if err := secondEvent.Sign(publisherSK); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(nostr.EventEnvelope{Event: secondEvent}); err != nil {
		t.Fatal(err)
	}
	assertOK(t, readWSMessage(t, conn))
}

func serveRelay(t *testing.T, handler http.Handler) (*httptest.Server, *websocket.Conn) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/relay", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return server, conn
}

func startPrivateRelay(t *testing.T, reader, publisher nostr.PubKey) *websocket.Conn {
	t.Helper()
	databasePath := t.TempDir() + "/relay.db"
	if publisher != nostr.ZeroPK {
		store, err := relaystore.Open(databasePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AllowPublisher(context.Background(), relaystore.Publisher{PubKey: publisher, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	resources, err := NewRelay(context.Background(), Config{Mode: ModePrivateSQLite, DatabasePath: databasePath, ServiceURL: "http://relay.example", ReaderPubkeys: []string{reader.Hex()}, MaxQueryLimit: 10})
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
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

func requestAuthChallenge(t *testing.T, conn *websocket.Conn, subscriptionID string) string {
	t.Helper()
	if err := conn.WriteJSON([]any{"REQ", subscriptionID, map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	message := readWSMessage(t, conn)
	if labelOf(t, message) != "AUTH" {
		t.Fatalf("response = %s, want AUTH", message[0])
	}
	var challenge string
	if err := json.Unmarshal(message[1], &challenge); err != nil {
		t.Fatal(err)
	}
	if message = readWSMessage(t, conn); labelOf(t, message) != "CLOSED" {
		t.Fatalf("response = %s, want CLOSED", message[0])
	}
	return challenge
}

func authenticate(t *testing.T, conn *websocket.Conn, sk nostr.SecretKey, challenge string) {
	t.Helper()
	authenticateForURL(t, conn, sk, challenge, "ws://relay.example")
}

func authenticateForURL(t *testing.T, conn *websocket.Conn, sk nostr.SecretKey, challenge, relayURL string) {
	t.Helper()
	event := nip42.CreateUnsignedAuthEvent(challenge, nostr.GetPublicKey(sk), relayURL)
	if err := event.Sign(sk); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(nostr.AuthEnvelope{Event: event}); err != nil {
		t.Fatal(err)
	}
	assertOK(t, readWSMessage(t, conn))
}

func assertOK(t *testing.T, message []json.RawMessage) {
	t.Helper()
	if labelOf(t, message) != "OK" {
		t.Fatalf("response = %s, want OK", message[0])
	}
	var accepted bool
	if err := json.Unmarshal(message[2], &accepted); err != nil {
		t.Fatal(err)
	}
	if !accepted {
		t.Fatalf("OK response rejected request: %s", message)
	}
}

func readWSMessage(t *testing.T, conn *websocket.Conn) []json.RawMessage {
	t.Helper()
	var message []json.RawMessage
	if err := conn.ReadJSON(&message); err != nil {
		t.Fatal(err)
	}
	return message
}

func labelOf(t *testing.T, message []json.RawMessage) string {
	t.Helper()
	var label string
	if err := json.Unmarshal(message[0], &label); err != nil {
		t.Fatal(err)
	}
	return label
}

func TestNIP01PublishAndQuery(t *testing.T) {
	resources, err := NewRelay(context.Background(), Config{MaxQueryLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resources.Close(); err != nil {
			t.Error(err)
		}
	})
	relay := resources.Relay

	server := httptest.NewServer(relay)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	relayURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client, err := nostr.RelayConnect(ctx, relayURL, nostr.RelayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	}()

	event := nostr.Event{
		CreatedAt: nostr.Now(),
		Kind:      nostr.Kind(1),
		Tags:      nostr.Tags{{"p", "0000000000000000000000000000000000000000000000000000000000000000"}},
		Content:   "hello from mytools",
	}
	if err := event.Sign(nostr.Generate()); err != nil {
		t.Fatal(err)
	}

	if err := client.Publish(ctx, event); err != nil {
		t.Fatal(err)
	}

	var found bool
	for got := range client.QueryEvents(nostr.Filter{IDs: []nostr.ID{event.ID}}) {
		found = true
		if got.ID != event.ID {
			t.Fatalf("event ID = %s, want %s", got.ID, event.ID)
		}
		if got.Content != event.Content {
			t.Fatalf("content = %q", got.Content)
		}
		break
	}
	if !found {
		t.Fatal("queried event was not returned")
	}
}

func TestNIP01LiveSubscriptionReceivesPublishedEvent(t *testing.T) {
	resources, err := NewRelay(context.Background(), Config{MaxQueryLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resources.Close(); err != nil {
			t.Error(err)
		}
	})
	relay := resources.Relay

	server := httptest.NewServer(relay)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	relayURL := "ws" + strings.TrimPrefix(server.URL, "http")
	subscriber, err := nostr.RelayConnect(ctx, relayURL, nostr.RelayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := subscriber.Close(); err != nil {
			t.Error(err)
		}
	}()

	publisher, err := nostr.RelayConnect(ctx, relayURL, nostr.RelayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := publisher.Close(); err != nil {
			t.Error(err)
		}
	}()

	sub, err := subscriber.Subscribe(ctx, nostr.Filter{Kinds: []nostr.Kind{nostr.Kind(1)}}, nostr.SubscriptionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsub()

	event := nostr.Event{
		CreatedAt: nostr.Now(),
		Kind:      nostr.Kind(1),
		Content:   "live hello",
	}
	if err := event.Sign(nostr.Generate()); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(ctx, event); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-sub.Events:
		if got.ID != event.ID {
			t.Fatalf("event ID = %s, want %s", got.ID, event.ID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for live event")
	}
}
