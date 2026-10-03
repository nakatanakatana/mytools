package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseRelays(t *testing.T) {
	input := map[string]string{
		"pub1": "wss://r1, wss://r2",
		"pub2": "wss://r3",
	}
	expected := map[string][]string{
		"pub1": {"wss://r1", "wss://r2"},
		"pub2": {"wss://r3"},
	}
	got := ParseRelays(input)
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("ParseRelays() = %v, want %v", got, expected)
	}
}

func TestMemoryProvider(t *testing.T) {
	mapping := map[string]string{
		"alice": "pub1",
		"bob":   "pub2",
	}
	relays := map[string]string{
		"pub1": "wss://r1, wss://r2",
	}

	provider, err := NewMemoryProvider(mapping, relays)
	if err != nil {
		t.Fatalf("NewMemoryProvider failed: %v", err)
	}

	// 1. Full response ("")
	fullBytes := provider.GetJSON("")
	if fullBytes == nil {
		t.Fatal("expected full response bytes, got nil")
	}
	var fullResp NIP05Response
	if err := json.Unmarshal(fullBytes, &fullResp); err != nil {
		t.Fatalf("failed to unmarshal full response: %v", err)
	}
	if len(fullResp.Names) != 2 || fullResp.Names["alice"] != "pub1" || fullResp.Names["bob"] != "pub2" {
		t.Errorf("unexpected names in full response: %v", fullResp.Names)
	}
	if len(fullResp.Relays) != 1 || len(fullResp.Relays["pub1"]) != 2 {
		t.Errorf("unexpected relays in full response: %v", fullResp.Relays)
	}

	// 2. Individual user with relays ("alice")
	aliceBytes := provider.GetJSON("alice")
	if aliceBytes == nil {
		t.Fatal("expected alice response bytes, got nil")
	}
	var aliceResp NIP05Response
	if err := json.Unmarshal(aliceBytes, &aliceResp); err != nil {
		t.Fatalf("failed to unmarshal alice response: %v", err)
	}
	if len(aliceResp.Names) != 1 || aliceResp.Names["alice"] != "pub1" {
		t.Errorf("unexpected names in alice response: %v", aliceResp.Names)
	}
	if len(aliceResp.Relays) != 1 || len(aliceResp.Relays["pub1"]) != 2 {
		t.Errorf("unexpected relays in alice response: %v", aliceResp.Relays)
	}

	// 3. Individual user without relays ("bob")
	bobBytes := provider.GetJSON("bob")
	if bobBytes == nil {
		t.Fatal("expected bob response bytes, got nil")
	}
	var bobResp NIP05Response
	if err := json.Unmarshal(bobBytes, &bobResp); err != nil {
		t.Fatalf("failed to unmarshal bob response: %v", err)
	}
	if len(bobResp.Names) != 1 || bobResp.Names["bob"] != "pub2" {
		t.Errorf("unexpected names in bob response: %v", bobResp.Names)
	}
	if len(bobResp.Relays) != 0 {
		t.Errorf("expected no relays in bob response, got %v", bobResp.Relays)
	}

	// 4. Unknown user ("unknown")
	unknownBytes := provider.GetJSON("unknown")
	if unknownBytes != nil {
		t.Errorf("expected nil for unknown user, got %s", string(unknownBytes))
	}
}