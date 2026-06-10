package main

import "testing"

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "0.0.0.0" {
		t.Fatalf("Host = %q", cfg.Host)
	}
	if cfg.Port != "8080" {
		t.Fatalf("Port = %q", cfg.Port)
	}
	if cfg.Name != "mytools relay" {
		t.Fatalf("Name = %q", cfg.Name)
	}
	if cfg.Description != "A minimal Nostr relay" {
		t.Fatalf("Description = %q", cfg.Description)
	}
	if cfg.MaxQueryLimit != 500 {
		t.Fatalf("MaxQueryLimit = %d", cfg.MaxQueryLimit)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	t.Setenv("NOSTR_RELAY_HOST", "127.0.0.1")
	t.Setenv("NOSTR_RELAY_PORT", "3334")
	t.Setenv("NOSTR_RELAY_NAME", "local relay")
	t.Setenv("NOSTR_RELAY_DESCRIPTION", "test relay")
	t.Setenv("NOSTR_RELAY_MAX_QUERY_LIMIT", "25")
	t.Setenv("LOG_LEVEL", "debug")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" {
		t.Fatalf("Host = %q", cfg.Host)
	}
	if cfg.Port != "3334" {
		t.Fatalf("Port = %q", cfg.Port)
	}
	if cfg.Name != "local relay" {
		t.Fatalf("Name = %q", cfg.Name)
	}
	if cfg.Description != "test relay" {
		t.Fatalf("Description = %q", cfg.Description)
	}
	if cfg.MaxQueryLimit != 25 {
		t.Fatalf("MaxQueryLimit = %d", cfg.MaxQueryLimit)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q", cfg.LogLevel)
	}
}
