package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestFileGenerator(t *testing.T) {
	mapping := map[string]string{"alice": "pub1", "bob": "pub2"}
	relays := map[string]string{"pub1": "wss://r1,wss://r2"}

	fg, err := NewFileGenerator(mapping, relays)
	if err != nil {
		t.Fatalf("NewFileGenerator failed: %v", err)
	}
	defer fg.Cleanup()

	// Test full file (empty name)
	fullPath := fg.GetFilePath("")
	if fullPath == "" {
		t.Fatal("Expected full file path, got empty")
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("Failed to read full file: %v", err)
	}
	var resp NIP05Response
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("Failed to unmarshal full file: %v", err)
	}
	if len(resp.Names) != 2 {
		t.Errorf("Expected 2 names in full response, got %d", len(resp.Names))
	}
	if len(resp.Relays) != 1 {
		t.Errorf("Expected 1 relay entry, got %d", len(resp.Relays))
	}

	// Test individual user file
	alicePath := fg.GetFilePath("alice")
	if alicePath == "" {
		t.Fatal("Expected alice file path, got empty")
	}
	data, err = os.ReadFile(alicePath)
	if err != nil {
		t.Fatalf("Failed to read alice file: %v", err)
	}
	var aliceResp NIP05Response
	if err := json.Unmarshal(data, &aliceResp); err != nil {
		t.Fatalf("Failed to unmarshal alice file: %v", err)
	}
	if len(aliceResp.Names) != 1 || aliceResp.Names["alice"] != "pub1" {
		t.Errorf("Expected alice only in response, got %v", aliceResp.Names)
	}
	if len(aliceResp.Relays) != 1 || len(aliceResp.Relays["pub1"]) != 2 {
		t.Errorf("Expected relays for pub1, got %v", aliceResp.Relays)
	}

	// Test unknown user
	unknownPath := fg.GetFilePath("unknown")
	if unknownPath != "" {
		t.Errorf("Expected empty path for unknown user, got %s", unknownPath)
	}
}
