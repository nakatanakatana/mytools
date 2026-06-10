package main

import "testing"

func TestServerAddress(t *testing.T) {
	cfg := Config{Host: "127.0.0.1", Port: "3334"}
	if got := ServerAddress(cfg); got != "127.0.0.1:3334" {
		t.Fatalf("ServerAddress() = %q", got)
	}
}
