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
)

func TestNIP11InformationDocument(t *testing.T) {
	relay, closer, err := NewRelay(Config{
		Name:          "test relay",
		Description:   "test description",
		MaxQueryLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closer()

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
		_ = resp.Body.Close()
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

func TestNIP01PublishAndQuery(t *testing.T) {
	relay, closer, err := NewRelay(Config{MaxQueryLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer closer()

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
		_ = client.Close()
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
	relay, closer, err := NewRelay(Config{MaxQueryLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer closer()

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
		_ = subscriber.Close()
	}()

	publisher, err := nostr.RelayConnect(ctx, relayURL, nostr.RelayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = publisher.Close()
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
