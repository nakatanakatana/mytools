package main

import "testing"

func TestNewRelaySetsInfo(t *testing.T) {
	cfg := Config{
		Name:          "test relay",
		Description:   "test description",
		MaxQueryLimit: 10,
	}

	relay, closer, err := NewRelay(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closer()

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

func containsNIP(values []any, want int) bool {
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
		}
	}
	return false
}
