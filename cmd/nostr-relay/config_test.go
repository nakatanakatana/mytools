package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoadConfigDefaultsToPublicMemory(t *testing.T) {
	t.Setenv("NOSTR_RELAY_MODE", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModePublicMemory {
		t.Fatalf("Mode = %q", cfg.Mode)
	}
}

func TestLoadConfigPrivateSQLiteRequiresPrivateSettings(t *testing.T) {
	t.Setenv("NOSTR_RELAY_MODE", "private-sqlite")
	t.Setenv("NOSTR_RELAY_DATABASE_PATH", "")
	_, err := LoadConfig()
	assertErrorContains(t, err, "NOSTR_RELAY_DATABASE_PATH")
}

func TestLoadConfigRejectsUnknownMode(t *testing.T) {
	t.Setenv("NOSTR_RELAY_MODE", "other")
	_, err := LoadConfig()
	assertErrorContains(t, err, "NOSTR_RELAY_MODE")
}

func TestLoadConfigRejectsInvalidListenerPorts(t *testing.T) {
	tests := []struct {
		name, variable, value string
	}{
		{name: "zero protocol port", variable: "NOSTR_RELAY_PORT", value: "0"},
		{name: "negative protocol port", variable: "NOSTR_RELAY_PORT", value: "-1"},
		{name: "protocol port above range", variable: "NOSTR_RELAY_PORT", value: "65536"},
		{name: "protocol service name", variable: "NOSTR_RELAY_PORT", value: "http"},
		{name: "zero management port", variable: "NOSTR_RELAY_MANAGEMENT_PORT", value: "0"},
		{name: "negative management port", variable: "NOSTR_RELAY_MANAGEMENT_PORT", value: "-1"},
		{name: "management port above range", variable: "NOSTR_RELAY_MANAGEMENT_PORT", value: "65536"},
		{name: "management service name", variable: "NOSTR_RELAY_MANAGEMENT_PORT", value: "http"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.variable, tt.value)
			_, err := LoadConfig()
			assertErrorContains(t, err, tt.variable)
		})
	}
}

func TestConfigRejectsEmptyListenerPorts(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "protocol", cfg: Config{Mode: ModePublicMemory, ManagementPort: "8081"}, want: "NOSTR_RELAY_PORT"},
		{name: "management", cfg: Config{Mode: ModePublicMemory, Port: "8080"}, want: "NOSTR_RELAY_MANAGEMENT_PORT"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertErrorContains(t, tt.cfg.validate(), tt.want)
		})
	}
}

func TestLoadConfigNormalizesBracketedIPv6ListenerHosts(t *testing.T) {
	t.Setenv("NOSTR_RELAY_HOST", "[::]")
	t.Setenv("NOSTR_RELAY_MANAGEMENT_HOST", "[::1]")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "::" || cfg.ManagementHost != "::1" {
		t.Fatalf("listener hosts = %q, %q", cfg.Host, cfg.ManagementHost)
	}
}

func TestLoadConfigRejectsBracketsAroundNonIPv6ListenerHost(t *testing.T) {
	t.Setenv("NOSTR_RELAY_HOST", "[localhost]")
	_, err := LoadConfig()
	assertErrorContains(t, err, "NOSTR_RELAY_HOST")
}

func TestValidateListenerHostIPv6Zones(t *testing.T) {
	tests := []struct {
		name, host, want string
		wantError        bool
	}{
		{name: "bare scoped IPv6", host: "fe80::1%eth-does-not-need-to-exist", want: "fe80::1%eth-does-not-need-to-exist"},
		{name: "bracketed scoped IPv6", host: "[fe80::1%eth-does-not-need-to-exist]", want: "fe80::1%eth-does-not-need-to-exist"},
		{name: "bare zone punctuation", host: "fe80::1%zone.name_1-2", want: "fe80::1%zone.name_1-2"},
		{name: "bracketed zone punctuation", host: "[fe80::1%zone.name_1-2]", want: "fe80::1%zone.name_1-2"},
		{name: "canonicalizes IPv6", host: "[0:0:0:0:0:0:0:1]", want: "::1"},
		{name: "empty", host: "   ", wantError: true},
		{name: "empty bracketed zone", host: "[::1%]", wantError: true},
		{name: "empty bare zone", host: "::1%", wantError: true},
		{name: "malformed colon host", host: "not:a:host", wantError: true},
		{name: "missing closing bracket", host: "[::1", wantError: true},
		{name: "missing opening bracket", host: "::1]", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateListenerHost("HOST", tt.host)
			if tt.wantError {
				assertErrorContains(t, err, "HOST")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("validateListenerHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateListenerHostRejectsInvalidIPv6Zones(t *testing.T) {
	invalidZones := []struct {
		name, zone string
	}{
		{name: "space", zone: "bad zone"},
		{name: "tab", zone: "bad\tzone"},
		{name: "newline", zone: "bad\nzone"},
		{name: "control", zone: "bad\x00zone"},
		{name: "unicode whitespace", zone: "bad\u00a0zone"},
		{name: "additional percent", zone: "bad%zone"},
	}
	for _, syntax := range []struct {
		name, format string
	}{
		{name: "bare", format: "fe80::1%%%s"},
		{name: "bracketed", format: "[fe80::1%%%s]"},
	} {
		for _, tt := range invalidZones {
			t.Run(syntax.name+"/"+tt.name, func(t *testing.T) {
				_, err := validateListenerHost("HOST", fmt.Sprintf(syntax.format, tt.zone))
				assertErrorContains(t, err, "HOST")
			})
		}
	}

	for _, host := range []string{
		" fe80::1%zone", "fe80::1%zone\t", "fe80::1%zone\n",
		" [fe80::1%zone]", "[fe80::1%zone]\t", "[fe80::1%zone]\n",
	} {
		t.Run("outer whitespace/"+host, func(t *testing.T) {
			_, err := validateListenerHost("HOST", host)
			assertErrorContains(t, err, "HOST")
		})
	}
}

func TestConfigValidateDoesNotMutateHostsOnFailure(t *testing.T) {
	cfg := Config{
		Host:           "[::1]",
		Port:           "8080",
		ManagementHost: "[::2]",
		ManagementPort: "8081",
		Mode:           ModePrivateSQLite,
	}
	wantHost, wantManagementHost := cfg.Host, cfg.ManagementHost
	assertErrorContains(t, cfg.validate(), "NOSTR_RELAY_DATABASE_PATH")
	if cfg.Host != wantHost || cfg.ManagementHost != wantManagementHost {
		t.Fatalf("failed validation mutated hosts to %q, %q; want %q, %q", cfg.Host, cfg.ManagementHost, wantHost, wantManagementHost)
	}
}

func TestLoadConfigPrivateSQLite(t *testing.T) {
	const admin = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const reader1 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const reader2 = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	t.Setenv("NOSTR_RELAY_MODE", "private-sqlite")
	t.Setenv("NOSTR_RELAY_DATABASE_PATH", "/var/lib/nostr-relay/relay.db")
	t.Setenv("NOSTR_RELAY_SERVICE_URL", "https://relay.example.com")
	t.Setenv("NOSTR_RELAY_ADMIN_PUBKEY", admin)
	t.Setenv("NOSTR_RELAY_READER_PUBKEYS", reader1+","+reader2)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModePrivateSQLite {
		t.Fatalf("Mode = %q", cfg.Mode)
	}
	if cfg.DatabasePath != "/var/lib/nostr-relay/relay.db" {
		t.Fatalf("DatabasePath = %q", cfg.DatabasePath)
	}
	if cfg.ServiceURL != "https://relay.example.com" {
		t.Fatalf("ServiceURL = %q", cfg.ServiceURL)
	}
	if cfg.AdminPubkey != admin {
		t.Fatalf("AdminPubkey = %q", cfg.AdminPubkey)
	}
	if len(cfg.ReaderPubkeys) != 2 || cfg.ReaderPubkeys[0] != reader1 || cfg.ReaderPubkeys[1] != reader2 {
		t.Fatalf("ReaderPubkeys = %#v", cfg.ReaderPubkeys)
	}
}

func TestLoadConfigPrivateSQLiteRejectsInvalidSettings(t *testing.T) {
	const admin = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const reader = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tests := []struct {
		name       string
		serviceURL string
		admin      string
		readers    string
		want       string
	}{
		{name: "missing service URL", serviceURL: "", admin: admin, readers: reader, want: "NOSTR_RELAY_SERVICE_URL"},
		{name: "relative service URL", serviceURL: "/relay", admin: admin, readers: reader, want: "NOSTR_RELAY_SERVICE_URL"},
		{name: "unsupported service URL scheme", serviceURL: "ftp://relay.example.com", admin: admin, readers: reader, want: "NOSTR_RELAY_SERVICE_URL"},
		{name: "service URL userinfo", serviceURL: "https://user@relay.example.com", admin: admin, readers: reader, want: "NOSTR_RELAY_SERVICE_URL"},
		{name: "service URL fragment", serviceURL: "https://relay.example.com/#admin", admin: admin, readers: reader, want: "NOSTR_RELAY_SERVICE_URL"},
		{name: "missing admin", serviceURL: "http://relay.example.com", admin: "", readers: reader, want: "NOSTR_RELAY_ADMIN_PUBKEY"},
		{name: "invalid admin hex", serviceURL: "http://relay.example.com", admin: "not-hex", readers: reader, want: "NOSTR_RELAY_ADMIN_PUBKEY"},
		{name: "missing readers", serviceURL: "http://relay.example.com", admin: admin, readers: "", want: "NOSTR_RELAY_READER_PUBKEYS"},
		{name: "empty reader", serviceURL: "http://relay.example.com", admin: admin, readers: reader + ",", want: "NOSTR_RELAY_READER_PUBKEYS"},
		{name: "invalid reader hex", serviceURL: "http://relay.example.com", admin: admin, readers: "not-hex", want: "NOSTR_RELAY_READER_PUBKEYS"},
		{name: "reader duplicates admin", serviceURL: "http://relay.example.com", admin: admin, readers: admin, want: "NOSTR_RELAY_READER_PUBKEYS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NOSTR_RELAY_MODE", "private-sqlite")
			t.Setenv("NOSTR_RELAY_DATABASE_PATH", "/tmp/relay.db")
			t.Setenv("NOSTR_RELAY_SERVICE_URL", tt.serviceURL)
			t.Setenv("NOSTR_RELAY_ADMIN_PUBKEY", tt.admin)
			t.Setenv("NOSTR_RELAY_READER_PUBKEYS", tt.readers)
			_, err := LoadConfig()
			assertErrorContains(t, err, tt.want)
		})
	}
}

func TestLoadConfigPrivateSQLiteManagementListenerSeparation(t *testing.T) {
	const admin = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const reader = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tests := []struct {
		name, protocolHost, protocolPort, managementHost, managementPort string
		wantError                                                        bool
	}{
		{name: "same address", protocolHost: "127.0.0.1", protocolPort: "8080", managementHost: "127.0.0.1", managementPort: "8080", wantError: true},
		{name: "same numeric port with leading zero", protocolHost: "127.0.0.1", protocolPort: "8080", managementHost: "127.0.0.1", managementPort: "08080", wantError: true},
		{name: "wildcard v4 and specific", protocolHost: "0.0.0.0", protocolPort: "8080", managementHost: "127.0.0.1", managementPort: "8080", wantError: true},
		{name: "wildcard v6 and specific", protocolHost: "::", protocolPort: "8080", managementHost: "::1", managementPort: "8080", wantError: true},
		{name: "same hostname normalized", protocolHost: "LOCALHOST.", protocolPort: "8080", managementHost: "localhost", managementPort: "8080", wantError: true},
		{name: "different hostnames are not provably distinct", protocolHost: "relay-a.local", protocolPort: "8080", managementHost: "relay-b.local", managementPort: "8080", wantError: true},
		{name: "different ports", protocolHost: "0.0.0.0", protocolPort: "8080", managementHost: "127.0.0.1", managementPort: "8081"},
		{name: "different specific addresses", protocolHost: "127.0.0.1", protocolPort: "8080", managementHost: "127.0.0.2", managementPort: "8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NOSTR_RELAY_MODE", "private-sqlite")
			t.Setenv("NOSTR_RELAY_HOST", tt.protocolHost)
			t.Setenv("NOSTR_RELAY_PORT", tt.protocolPort)
			t.Setenv("NOSTR_RELAY_MANAGEMENT_HOST", tt.managementHost)
			t.Setenv("NOSTR_RELAY_MANAGEMENT_PORT", tt.managementPort)
			t.Setenv("NOSTR_RELAY_DATABASE_PATH", "/tmp/relay.db")
			t.Setenv("NOSTR_RELAY_SERVICE_URL", "https://relay.example.com")
			t.Setenv("NOSTR_RELAY_ADMIN_PUBKEY", admin)
			t.Setenv("NOSTR_RELAY_READER_PUBKEYS", reader)

			_, err := LoadConfig()
			if tt.wantError {
				assertErrorContains(t, err, "must select a listener distinct")
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func assertErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err, want)
	}
}

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
	if cfg.ManagementHost != "0.0.0.0" || cfg.ManagementPort != "8081" {
		t.Fatalf("management listener = %s:%s", cfg.ManagementHost, cfg.ManagementPort)
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
