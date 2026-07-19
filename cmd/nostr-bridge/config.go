package main

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/caarlos0/env/v11"
)

// Config contains the settings required to start nostr-bridge.
type Config struct {
	Host                        string        `env:"NOSTR_BRIDGE_HOST" envDefault:"127.0.0.1"`
	Port                        string        `env:"NOSTR_BRIDGE_PORT" envDefault:"8080"`
	DatabasePath                string        `env:"NOSTR_BRIDGE_DATABASE_PATH,required"`
	RelayURL                    string        `env:"NOSTR_BRIDGE_RELAY_URL,required"`
	RelayManagementURL          string        `env:"NOSTR_BRIDGE_RELAY_MANAGEMENT_URL,required"`
	RelayCanonicalURL           string        `env:"NOSTR_BRIDGE_RELAY_CANONICAL_URL,required"`
	RelayAdminPrivateKey        string        `env:"NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY,required"`
	OutboxLimit                 int           `env:"NOSTR_BRIDGE_OUTBOX_LIMIT" envDefault:"10000"`
	OutboxPollInterval          time.Duration `env:"NOSTR_BRIDGE_OUTBOX_POLL_INTERVAL" envDefault:"1s"`
	OAuthCallbackURL            string        `env:"NOSTR_BRIDGE_OAUTH_CALLBACK_URL,required"`
	OAuthAuthorizationServerURL string        `env:"NOSTR_BRIDGE_OAUTH_AUTHORIZATION_SERVER_URL,required"`
	OAuthClientID               string        `env:"NOSTR_BRIDGE_OAUTH_CLIENT_ID,required"`
	OAuthClientSigningKey       string        `env:"NOSTR_BRIDGE_OAUTH_CLIENT_SIGNING_KEY,required"`
	OAuthEncryptionKey          string        `env:"NOSTR_BRIDGE_OAUTH_ENCRYPTION_KEY,required"`
	AccountDID                  string        `env:"NOSTR_BRIDGE_ACCOUNT_DID,required"`
	BlueskyBaseURL              string        `env:"NOSTR_BRIDGE_BLUESKY_BASE_URL,required"`
	MasterSeed                  string        `env:"NOSTR_BRIDGE_MASTER_SEED,required"`
	JetstreamURL                string        `env:"NOSTR_BRIDGE_JETSTREAM_URL,required"`
	ListURIs                    []string      `env:"NOSTR_BRIDGE_LIST_URIS" envSeparator:","`
	BackfillLimit               int           `env:"NOSTR_BRIDGE_BACKFILL_LIMIT" envDefault:"100"`
	ReconcileInterval           time.Duration `env:"NOSTR_BRIDGE_RECONCILE_INTERVAL" envDefault:"1h"`
}

// LoadConfig reads Config from the process environment.
func LoadConfig() (Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(cfg.DatabasePath) == "" {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_DATABASE_PATH must not be empty")
	}
	if strings.TrimSpace(cfg.OAuthCallbackURL) == "" {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_OAUTH_CALLBACK_URL must not be empty")
	}
	for _, setting := range []struct {
		name  string
		value string
	}{
		{"NOSTR_BRIDGE_OAUTH_AUTHORIZATION_SERVER_URL", cfg.OAuthAuthorizationServerURL},
		{"NOSTR_BRIDGE_OAUTH_CLIENT_ID", cfg.OAuthClientID},
		{"NOSTR_BRIDGE_OAUTH_CLIENT_SIGNING_KEY", cfg.OAuthClientSigningKey},
		{"NOSTR_BRIDGE_OAUTH_ENCRYPTION_KEY", cfg.OAuthEncryptionKey},
		{"NOSTR_BRIDGE_ACCOUNT_DID", cfg.AccountDID},
		{"NOSTR_BRIDGE_BLUESKY_BASE_URL", cfg.BlueskyBaseURL},
		{"NOSTR_BRIDGE_MASTER_SEED", cfg.MasterSeed},
		{"NOSTR_BRIDGE_JETSTREAM_URL", cfg.JetstreamURL},
	} {
		if strings.TrimSpace(setting.value) == "" {
			return Config{}, fmt.Errorf("%s must not be empty", setting.name)
		}
	}
	if !validEndpoint(cfg.RelayURL, "ws", "wss") {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_RELAY_URL must be an absolute ws/wss URL")
	}
	if !validEndpoint(cfg.RelayManagementURL, "http", "https") {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_RELAY_MANAGEMENT_URL must be an absolute http/https URL")
	}
	if !validEndpoint(cfg.RelayCanonicalURL, "http", "https") {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_RELAY_CANONICAL_URL must be an absolute http/https URL")
	}
	if _, err := nostr.SecretKeyFromHex(cfg.RelayAdminPrivateKey); err != nil {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY must be valid secret hex")
	}
	seed, err := base64.StdEncoding.DecodeString(cfg.MasterSeed)
	if err != nil || len(seed) != 32 {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_MASTER_SEED must be base64 encoding of exactly 32 bytes")
	}
	if cfg.OutboxLimit <= 0 {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_OUTBOX_LIMIT must be positive")
	}
	if cfg.OutboxPollInterval <= 0 {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_OUTBOX_POLL_INTERVAL must be positive")
	}
	return cfg, nil
}

func validEndpoint(raw string, schemes ...string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	for _, scheme := range schemes {
		if u.Scheme == scheme {
			return true
		}
	}
	return false
}
