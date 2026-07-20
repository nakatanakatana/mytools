package main

import (
	"context"
	"strconv"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	relaystore "github.com/nakatanakatana/mytools/cmd/nostr-relay/store"
)

func TestNewRelaySetsInfo(t *testing.T) {
	cfg := Config{
		Name:          "test relay",
		Description:   "test description",
		MaxQueryLimit: 10,
	}

	resources, err := NewRelay(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resources.Close(); err != nil {
			t.Error(err)
		}
	})
	relay := resources.Relay

	if relay.Info.Name != "test relay" {
		t.Fatalf("Name = %q", relay.Info.Name)
	}
	if relay.Info.Description != "test description" {
		t.Fatalf("Description = %q", relay.Info.Description)
	}
	if len(relay.Info.SupportedNIPs) == 0 {
		t.Fatal("SupportedNIPs is empty")
	}
	if !containsNIP(relay.Info.SupportedNIPs, 1) {
		t.Fatalf("SupportedNIPs = %v, want NIP-01", relay.Info.SupportedNIPs)
	}
	if !containsNIP(relay.Info.SupportedNIPs, 11) {
		t.Fatalf("SupportedNIPs = %v, want NIP-11", relay.Info.SupportedNIPs)
	}
}

func TestPublicMemoryAllowsUnauthenticatedRequestsAndPublishing(t *testing.T) {
	resources, err := NewRelay(context.Background(), Config{Mode: ModePublicMemory, MaxQueryLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resources.Close(); err != nil {
			t.Error(err)
		}
	})

	if resources.Relay.OnRequest != nil || resources.Relay.OnEvent != nil {
		t.Fatal("public-memory relay unexpectedly installed authorization hooks")
	}
}

func TestRelaySupportedNIPsAreModeSpecific(t *testing.T) {
	reader := nostr.GetPublicKey(nostr.Generate())
	tests := []struct {
		name string
		cfg  Config
		want []int
	}{
		{name: "public-memory", cfg: Config{Mode: ModePublicMemory}, want: []int{1, 11, 40}},
		{name: "private-sqlite", cfg: Config{Mode: ModePrivateSQLite, DatabasePath: t.TempDir() + "/relay.db", ServiceURL: "https://relay.example", ReaderPubkeys: []string{reader.Hex()}}, want: []int{1, 9, 11, 42, 86, 98}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resources, err := NewRelay(context.Background(), tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := resources.Close(); err != nil {
					t.Error(err)
				}
			})
			if len(resources.Relay.Info.SupportedNIPs) != len(tt.want) {
				t.Fatalf("SupportedNIPs = %v, want %v", resources.Relay.Info.SupportedNIPs, tt.want)
			}
			for i, want := range tt.want {
				if !containsNIP([]any{resources.Relay.Info.SupportedNIPs[i]}, want) {
					t.Fatalf("SupportedNIPs = %v, want %v", resources.Relay.Info.SupportedNIPs, tt.want)
				}
			}
		})
	}
}

func TestPrivateSQLiteReaderAuthorization(t *testing.T) {
	readerSK := nostr.Generate()
	reader := nostr.GetPublicKey(readerSK)
	publisher := nostr.GetPublicKey(nostr.Generate())
	outsider := nostr.GetPublicKey(nostr.Generate())
	resources := newPrivateTestRelay(t, reader, publisher)

	tests := []struct {
		name   string
		ctx    context.Context
		reject bool
	}{
		{name: "unauthenticated", ctx: context.Background(), reject: true},
		{name: "reader allowlist", ctx: khatru.ForceSetAuthed(context.Background(), reader), reject: false},
		{name: "publisher only", ctx: khatru.ForceSetAuthed(context.Background(), publisher), reject: true},
		{name: "outsider", ctx: khatru.ForceSetAuthed(context.Background(), outsider), reject: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reject, _ := resources.Relay.OnRequest(tt.ctx, nostr.Filter{})
			if reject != tt.reject {
				t.Fatalf("reject = %v, want %v", reject, tt.reject)
			}
		})
	}
}

func TestPrivateSQLitePublisherAuthorization(t *testing.T) {
	readerSK := nostr.Generate()
	reader := nostr.GetPublicKey(readerSK)
	publisherSK := nostr.Generate()
	publisher := nostr.GetPublicKey(publisherSK)
	outsiderSK := nostr.Generate()
	resources := newPrivateTestRelay(t, reader, publisher)

	signed := func(sk nostr.SecretKey) nostr.Event {
		event := nostr.Event{CreatedAt: nostr.Now(), Kind: 1, Content: "test"}
		if err := event.Sign(sk); err != nil {
			t.Fatal(err)
		}
		return event
	}
	invalid := signed(publisherSK)
	invalid.Content = "tampered"

	tests := []struct {
		name   string
		event  nostr.Event
		reject bool
	}{
		{name: "publisher allowlist", event: signed(publisherSK), reject: false},
		{name: "reader only", event: signed(readerSK), reject: true},
		{name: "outsider", event: signed(outsiderSK), reject: true},
		{name: "invalid signature", event: invalid, reject: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reject, _ := resources.Relay.OnEvent(context.Background(), tt.event)
			if reject != tt.reject {
				t.Fatalf("reject = %v, want %v", reject, tt.reject)
			}
		})
	}

	event := signed(publisherSK)
	if _, err := resources.Relay.AddEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	var stored nostr.Event
	for got := range resources.Relay.QueryStored(context.Background(), nostr.Filter{IDs: []nostr.ID{event.ID}}) {
		stored = got
		break
	}
	if stored.ID != event.ID {
		t.Fatalf("stored ID = %s, want %s", stored.ID, event.ID)
	}
}

type privateTestResources struct {
	Relay *khatru.Relay
}

func newPrivateTestRelay(t *testing.T, reader, publisher nostr.PubKey) privateTestResources {
	t.Helper()
	databasePath := t.TempDir() + "/relay.db"
	store, err := relaystore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	allowPublisher(t, store, publisher)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	resources, err := NewRelay(context.Background(), Config{
		Mode: ModePrivateSQLite, DatabasePath: databasePath,
		ServiceURL: "https://relay.example", ReaderPubkeys: []string{reader.Hex()}, MaxQueryLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resources.Close(); err != nil {
			t.Error(err)
		}
	})
	return privateTestResources{Relay: resources.Relay}
}

func allowPublisher(t *testing.T, store *relaystore.SQLiteStore, pubkey nostr.PubKey) {
	t.Helper()
	if err := store.AllowPublisher(context.Background(), relaystore.Publisher{PubKey: pubkey, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func containsNIP(values []any, want int) bool {
	wantStr := strconv.Itoa(want)
	for _, value := range values {
		switch typed := value.(type) {
		case int:
			if typed == want {
				return true
			}
		case float64:
			if int(typed) == want {
				return true
			}
		case string:
			if typed == wantStr {
				return true
			}
		}
	}
	return false
}
