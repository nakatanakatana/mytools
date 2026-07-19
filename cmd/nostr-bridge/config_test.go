package main

import (
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv("NOSTR_BRIDGE_HOST", "127.0.0.1")
	t.Setenv("NOSTR_BRIDGE_PORT", "4321")
	t.Setenv("NOSTR_BRIDGE_DATABASE_PATH", "/tmp/nostr-bridge.db")
	t.Setenv("NOSTR_BRIDGE_RELAY_URL", "wss://relay.example")
	t.Setenv("NOSTR_BRIDGE_RELAY_MANAGEMENT_URL", "https://relay.example/manage")
	t.Setenv("NOSTR_BRIDGE_RELAY_CANONICAL_URL", "https://relay.example/manage")
	t.Setenv("NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY", "1111111111111111111111111111111111111111111111111111111111111111")
	t.Setenv("NOSTR_BRIDGE_OAUTH_CALLBACK_URL", "https://bridge.example/oauth/callback")
	t.Setenv("NOSTR_BRIDGE_OAUTH_AUTHORIZATION_SERVER_URL", "https://oauth.example")
	t.Setenv("NOSTR_BRIDGE_OAUTH_CLIENT_ID", "https://bridge.example/oauth/client-metadata.json")
	t.Setenv("NOSTR_BRIDGE_OAUTH_CLIENT_SIGNING_KEY", "test-secret")
	t.Setenv("NOSTR_BRIDGE_OAUTH_ENCRYPTION_KEY", "test-secret")
	t.Setenv("NOSTR_BRIDGE_ACCOUNT_DID", "did:plc:owner")
	t.Setenv("NOSTR_BRIDGE_BLUESKY_BASE_URL", "https://bsky.example")
	t.Setenv("NOSTR_BRIDGE_MASTER_SEED", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("NOSTR_BRIDGE_JETSTREAM_URL", "wss://jetstream.example/subscribe")
	t.Setenv("NOSTR_BRIDGE_LIST_URIS", "at://did:plc:one/app.bsky.graph.list/one,at://did:plc:two/app.bsky.graph.list/two")
	t.Setenv("NOSTR_BRIDGE_BACKFILL_LIMIT", "25")
	t.Setenv("NOSTR_BRIDGE_RECONCILE_INTERVAL", "15m")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Host != "127.0.0.1" || cfg.Port != "4321" {
		t.Fatalf("listener = %q:%q", cfg.Host, cfg.Port)
	}
	if cfg.DatabasePath != "/tmp/nostr-bridge.db" || cfg.RelayURL != "wss://relay.example" {
		t.Fatalf("database or relay config was not loaded: %#v", cfg)
	}
	if cfg.RelayManagementURL != "https://relay.example/manage" || cfg.RelayCanonicalURL != "https://relay.example/manage" || cfg.OutboxLimit != 10000 || cfg.OutboxPollInterval != time.Second {
		t.Fatalf("external relay config = %#v", cfg)
	}
	if cfg.OAuthCallbackURL != "https://bridge.example/oauth/callback" || cfg.JetstreamURL != "wss://jetstream.example/subscribe" || cfg.AccountDID != "did:plc:owner" || cfg.BlueskyBaseURL != "https://bsky.example" || cfg.MasterSeed != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" {
		t.Fatalf("OAuth or Jetstream config was not loaded: %#v", cfg)
	}
	if len(cfg.ListURIs) != 2 || cfg.ListURIs[0] != "at://did:plc:one/app.bsky.graph.list/one" || cfg.ListURIs[1] != "at://did:plc:two/app.bsky.graph.list/two" {
		t.Fatalf("ListURIs = %#v", cfg.ListURIs)
	}
	if cfg.BackfillLimit != 25 || cfg.ReconcileInterval != 15*time.Minute {
		t.Fatalf("backfill or reconcile config was not loaded: %#v", cfg)
	}
}

func TestLoadConfigRejectsMissingRequiredValues(t *testing.T) {
	for _, variable := range []string{
		"NOSTR_BRIDGE_DATABASE_PATH",
		"NOSTR_BRIDGE_OAUTH_CALLBACK_URL",
		"NOSTR_BRIDGE_OAUTH_AUTHORIZATION_SERVER_URL",
		"NOSTR_BRIDGE_OAUTH_CLIENT_ID",
		"NOSTR_BRIDGE_OAUTH_CLIENT_SIGNING_KEY",
		"NOSTR_BRIDGE_OAUTH_ENCRYPTION_KEY",
		"NOSTR_BRIDGE_ACCOUNT_DID",
		"NOSTR_BRIDGE_BLUESKY_BASE_URL",
		"NOSTR_BRIDGE_MASTER_SEED",
		"NOSTR_BRIDGE_JETSTREAM_URL",
		"NOSTR_BRIDGE_RELAY_URL",
		"NOSTR_BRIDGE_RELAY_MANAGEMENT_URL",
		"NOSTR_BRIDGE_RELAY_CANONICAL_URL",
		"NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY",
	} {
		t.Run(variable, func(t *testing.T) {
			t.Setenv("NOSTR_BRIDGE_DATABASE_PATH", "/tmp/nostr-bridge.db")
			t.Setenv("NOSTR_BRIDGE_OAUTH_CALLBACK_URL", "https://bridge.example/oauth/callback")
			t.Setenv("NOSTR_BRIDGE_OAUTH_AUTHORIZATION_SERVER_URL", "https://oauth.example")
			t.Setenv("NOSTR_BRIDGE_OAUTH_CLIENT_ID", "https://bridge.example/oauth/client-metadata.json")
			t.Setenv("NOSTR_BRIDGE_OAUTH_CLIENT_SIGNING_KEY", "test-secret")
			t.Setenv("NOSTR_BRIDGE_OAUTH_ENCRYPTION_KEY", "test-secret")
			t.Setenv("NOSTR_BRIDGE_ACCOUNT_DID", "did:plc:owner")
			t.Setenv("NOSTR_BRIDGE_BLUESKY_BASE_URL", "https://bsky.example")
			t.Setenv("NOSTR_BRIDGE_MASTER_SEED", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			t.Setenv("NOSTR_BRIDGE_JETSTREAM_URL", "wss://jetstream.example")
			t.Setenv("NOSTR_BRIDGE_JETSTREAM_URL", "wss://jetstream.example/subscribe")
			t.Setenv("NOSTR_BRIDGE_RELAY_URL", "wss://relay.example")
			t.Setenv("NOSTR_BRIDGE_RELAY_MANAGEMENT_URL", "https://relay.example/manage")
			t.Setenv("NOSTR_BRIDGE_RELAY_CANONICAL_URL", "https://relay.example/manage")
			t.Setenv("NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY", "1111111111111111111111111111111111111111111111111111111111111111")
			t.Setenv(variable, "")

			if _, err := LoadConfig(); err == nil {
				t.Fatalf("LoadConfig() succeeded without %s", variable)
			}
		})
	}
}

func TestLoadConfigRejectsInvalidExternalRelaySettings(t *testing.T) {
	for _, relayURL := range []string{"https://relay.example", "/relative", "wss://user@relay.example", "wss://relay.example/#fragment"} {
		t.Run(relayURL, func(t *testing.T) {
			t.Setenv("NOSTR_BRIDGE_DATABASE_PATH", "/tmp/nostr-bridge.db")
			t.Setenv("NOSTR_BRIDGE_OAUTH_CALLBACK_URL", "https://bridge.example/oauth/callback")
			t.Setenv("NOSTR_BRIDGE_OAUTH_AUTHORIZATION_SERVER_URL", "https://oauth.example")
			t.Setenv("NOSTR_BRIDGE_OAUTH_CLIENT_ID", "https://bridge.example/oauth/client-metadata.json")
			t.Setenv("NOSTR_BRIDGE_OAUTH_CLIENT_SIGNING_KEY", "test-secret")
			t.Setenv("NOSTR_BRIDGE_OAUTH_ENCRYPTION_KEY", "test-secret")
			t.Setenv("NOSTR_BRIDGE_ACCOUNT_DID", "did:plc:owner")
			t.Setenv("NOSTR_BRIDGE_BLUESKY_BASE_URL", "https://bsky.example")
			t.Setenv("NOSTR_BRIDGE_MASTER_SEED", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			t.Setenv("NOSTR_BRIDGE_RELAY_URL", relayURL)
			t.Setenv("NOSTR_BRIDGE_RELAY_MANAGEMENT_URL", "https://relay.example/manage")
			t.Setenv("NOSTR_BRIDGE_RELAY_CANONICAL_URL", "https://relay.example/manage")
			t.Setenv("NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY", "1111111111111111111111111111111111111111111111111111111111111111")

			if _, err := LoadConfig(); err == nil {
				t.Fatalf("LoadConfig() succeeded with invalid relay URL %q", relayURL)
			}
		})
	}
}

func TestValidEndpointRejectsCredentialsAndFragmentsButAllowsPathAndQuery(t *testing.T) {
	for _, raw := range []string{"wss://user@relay.example", "wss://relay.example/#fragment"} {
		if validEndpoint(raw, "ws", "wss") {
			t.Fatalf("validEndpoint(%q) = true", raw)
		}
	}
	if !validEndpoint("wss://relay.example/path?token=bound", "ws", "wss") {
		t.Fatal("path and query endpoint rejected")
	}
}

func TestLoadConfigRejectsMasterSeedThatIsNotExactly32Bytes(t *testing.T) {
	for _, seed := range []string{"c2VlZA==", "not-base64", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="} {
		t.Run(seed, func(t *testing.T) {
			t.Setenv("NOSTR_BRIDGE_DATABASE_PATH", "/tmp/nostr-bridge.db")
			t.Setenv("NOSTR_BRIDGE_RELAY_URL", "wss://relay.example")
			t.Setenv("NOSTR_BRIDGE_RELAY_MANAGEMENT_URL", "https://relay.example/manage")
			t.Setenv("NOSTR_BRIDGE_RELAY_CANONICAL_URL", "https://relay.example/manage")
			t.Setenv("NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY", "1111111111111111111111111111111111111111111111111111111111111111")
			t.Setenv("NOSTR_BRIDGE_OAUTH_CALLBACK_URL", "https://bridge.example/oauth/callback")
			t.Setenv("NOSTR_BRIDGE_OAUTH_AUTHORIZATION_SERVER_URL", "https://oauth.example")
			t.Setenv("NOSTR_BRIDGE_OAUTH_CLIENT_ID", "https://bridge.example/client")
			t.Setenv("NOSTR_BRIDGE_OAUTH_CLIENT_SIGNING_KEY", "key")
			t.Setenv("NOSTR_BRIDGE_OAUTH_ENCRYPTION_KEY", "key")
			t.Setenv("NOSTR_BRIDGE_ACCOUNT_DID", "did:plc:owner")
			t.Setenv("NOSTR_BRIDGE_BLUESKY_BASE_URL", "https://bsky.example")
			t.Setenv("NOSTR_BRIDGE_MASTER_SEED", seed)
			t.Setenv("NOSTR_BRIDGE_JETSTREAM_URL", "wss://jetstream.example")
			if _, err := LoadConfig(); err == nil {
				t.Fatal("LoadConfig() accepted invalid seed length")
			}
		})
	}
}
