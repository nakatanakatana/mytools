package main

import (
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