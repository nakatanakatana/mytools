package main

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/caarlos0/env/v11"
)

type SharedConfig struct {
	Host                 string        `env:"NOSTR_BRIDGE_HOST" envDefault:"127.0.0.1"`
	Port                 string        `env:"NOSTR_BRIDGE_PORT" envDefault:"8080"`
	DatabasePath         string        `env:"NOSTR_BRIDGE_DATABASE_PATH,required"`
	MasterSeed           string        `env:"NOSTR_BRIDGE_MASTER_SEED,required"`
	RelayURL             string        `env:"NOSTR_BRIDGE_RELAY_URL,required"`
	RelayManagementURL   string        `env:"NOSTR_BRIDGE_RELAY_MANAGEMENT_URL,required"`
	RelayCanonicalURL    string        `env:"NOSTR_BRIDGE_RELAY_CANONICAL_URL,required"`
	RelayAdminPrivateKey string        `env:"NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY,required"`
	OutboxLimit          int           `env:"NOSTR_BRIDGE_OUTBOX_LIMIT" envDefault:"10000"`
	OutboxPollInterval   time.Duration `env:"NOSTR_BRIDGE_OUTBOX_POLL_INTERVAL" envDefault:"1s"`
}

type OwnerConfig struct {
	ID      string `env:"NOSTR_BRIDGE_OWNER_ID,required"`
	Name    string `env:"NOSTR_BRIDGE_OWNER_NAME" envDefault:"nostr-bridge"`
	About   string `env:"NOSTR_BRIDGE_OWNER_ABOUT"`
	Picture string `env:"NOSTR_BRIDGE_OWNER_PICTURE"`
}

type BlueskyConfig struct {
	AccountDID                  string        `env:"NOSTR_BRIDGE_BLUESKY_ACCOUNT_DID"`
	BaseURL                     string        `env:"NOSTR_BRIDGE_BLUESKY_BASE_URL"`
	JetstreamURL                string        `env:"NOSTR_BRIDGE_BLUESKY_JETSTREAM_URL"`
	ListURIs                    []string      `env:"NOSTR_BRIDGE_BLUESKY_LIST_URIS" envSeparator:","`
	BackfillLimit               int           `env:"NOSTR_BRIDGE_BLUESKY_BACKFILL_LIMIT" envDefault:"100"`
	ReconcileInterval           time.Duration `env:"NOSTR_BRIDGE_BLUESKY_RECONCILE_INTERVAL" envDefault:"1h"`
	OAuthCallbackURL            string        `env:"NOSTR_BRIDGE_BLUESKY_OAUTH_CALLBACK_URL"`
	OAuthAuthorizationServerURL string        `env:"NOSTR_BRIDGE_BLUESKY_OAUTH_AUTHORIZATION_SERVER_URL"`
	OAuthClientID               string        `env:"NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_ID"`
	OAuthClientSigningKey       string        `env:"NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_SIGNING_KEY"`
	OAuthEncryptionKey          string        `env:"NOSTR_BRIDGE_BLUESKY_OAUTH_ENCRYPTION_KEY"`
}

func (c BlueskyConfig) Enabled() bool { return strings.TrimSpace(c.BaseURL) != "" }

type MastodonConfig struct {
	BaseURL            string        `env:"NOSTR_BRIDGE_MASTODON_BASE_URL"`
	Account            string        `env:"NOSTR_BRIDGE_MASTODON_ACCOUNT"`
	ListIDs            []string      `env:"NOSTR_BRIDGE_MASTODON_LIST_IDS" envSeparator:","`
	BackfillLimit      int           `env:"NOSTR_BRIDGE_MASTODON_BACKFILL_LIMIT" envDefault:"100"`
	ReconcileInterval  time.Duration `env:"NOSTR_BRIDGE_MASTODON_RECONCILE_INTERVAL" envDefault:"1h"`
	OAuthCallbackURL   string        `env:"NOSTR_BRIDGE_MASTODON_OAUTH_CALLBACK_URL"`
	OAuthClientID      string        `env:"NOSTR_BRIDGE_MASTODON_OAUTH_CLIENT_ID"`
	OAuthClientSecret  string        `env:"NOSTR_BRIDGE_MASTODON_OAUTH_CLIENT_SECRET"`
	OAuthEncryptionKey string        `env:"NOSTR_BRIDGE_MASTODON_OAUTH_ENCRYPTION_KEY"`
}

func (c MastodonConfig) Enabled() bool { return strings.TrimSpace(c.BaseURL) != "" }

// Config contains grouped settings required to start nostr-bridge.
type Config struct {
	Shared   SharedConfig
	Owner    OwnerConfig
	Bluesky  BlueskyConfig
	Mastodon MastodonConfig
}

type configDocumentation uint8

const (
	documentInReadme configDocumentation = iota
	documentInReadmeAndDeployment
)

type configVariable struct {
	name          string
	documentation configDocumentation
	removed       bool
}

// configVariables is the production-owned inventory used for removed-variable
// rejection and for keeping operator documentation aligned with LoadConfig.
var configVariables = []configVariable{
	{name: "NOSTR_BRIDGE_HOST", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_PORT", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_DATABASE_PATH", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_MASTER_SEED", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_RELAY_URL", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_RELAY_MANAGEMENT_URL", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_RELAY_CANONICAL_URL", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_OUTBOX_LIMIT", documentation: documentInReadme},
	{name: "NOSTR_BRIDGE_OUTBOX_POLL_INTERVAL", documentation: documentInReadme},
	{name: "NOSTR_BRIDGE_OWNER_ID", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_OWNER_NAME", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_OWNER_ABOUT", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_OWNER_PICTURE", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_BLUESKY_ACCOUNT_DID", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_BLUESKY_BASE_URL", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_BLUESKY_JETSTREAM_URL", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_BLUESKY_LIST_URIS", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_BLUESKY_BACKFILL_LIMIT", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_BLUESKY_RECONCILE_INTERVAL", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_BLUESKY_OAUTH_CALLBACK_URL", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_BLUESKY_OAUTH_AUTHORIZATION_SERVER_URL", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_ID", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_SIGNING_KEY", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_BLUESKY_OAUTH_ENCRYPTION_KEY", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_MASTODON_BASE_URL", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_MASTODON_ACCOUNT", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_MASTODON_LIST_IDS", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_MASTODON_BACKFILL_LIMIT", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_MASTODON_RECONCILE_INTERVAL", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_MASTODON_OAUTH_CALLBACK_URL", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_MASTODON_OAUTH_CLIENT_ID", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_MASTODON_OAUTH_CLIENT_SECRET", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_MASTODON_OAUTH_ENCRYPTION_KEY", documentation: documentInReadmeAndDeployment},
	{name: "NOSTR_BRIDGE_ACCOUNT_DID", removed: true},
	{name: "NOSTR_BRIDGE_JETSTREAM_URL", removed: true},
	{name: "NOSTR_BRIDGE_LIST_URIS", removed: true},
	{name: "NOSTR_BRIDGE_BACKFILL_LIMIT", removed: true},
	{name: "NOSTR_BRIDGE_RECONCILE_INTERVAL", removed: true},
	{name: "NOSTR_BRIDGE_OAUTH_CALLBACK_URL", removed: true},
	{name: "NOSTR_BRIDGE_OAUTH_AUTHORIZATION_SERVER_URL", removed: true},
	{name: "NOSTR_BRIDGE_OAUTH_CLIENT_ID", removed: true},
	{name: "NOSTR_BRIDGE_OAUTH_CLIENT_SIGNING_KEY", removed: true},
	{name: "NOSTR_BRIDGE_OAUTH_ENCRYPTION_KEY", removed: true},
}

func LoadConfig() (Config, error) {
	for _, variable := range configVariables {
		if variable.removed {
			if _, exists := os.LookupEnv(variable.name); exists {
				return Config{}, fmt.Errorf("removed configuration variable %s is set", variable.name)
			}
		}
	}
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(cfg.Shared.DatabasePath) == "" {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_DATABASE_PATH must not be empty")
	}
	if strings.TrimSpace(cfg.Owner.ID) == "" {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_OWNER_ID must not be empty")
	}
	if cfg.Owner.Picture != "" && !validEndpoint(cfg.Owner.Picture, "https") {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_OWNER_PICTURE must be an absolute HTTPS URL")
	}
	if !cfg.Bluesky.Enabled() && !cfg.Mastodon.Enabled() {
		return Config{}, fmt.Errorf("at least one provider must be enabled")
	}
	if cfg.Bluesky.Enabled() {
		if err := requireSettings([]setting{
			{"NOSTR_BRIDGE_BLUESKY_ACCOUNT_DID", cfg.Bluesky.AccountDID},
			{"NOSTR_BRIDGE_BLUESKY_JETSTREAM_URL", cfg.Bluesky.JetstreamURL},
			{"NOSTR_BRIDGE_BLUESKY_OAUTH_CALLBACK_URL", cfg.Bluesky.OAuthCallbackURL},
			{"NOSTR_BRIDGE_BLUESKY_OAUTH_AUTHORIZATION_SERVER_URL", cfg.Bluesky.OAuthAuthorizationServerURL},
			{"NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_ID", cfg.Bluesky.OAuthClientID},
			{"NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_SIGNING_KEY", cfg.Bluesky.OAuthClientSigningKey},
			{"NOSTR_BRIDGE_BLUESKY_OAUTH_ENCRYPTION_KEY", cfg.Bluesky.OAuthEncryptionKey},
		}); err != nil {
			return Config{}, err
		}
		if err := requireOAuthRoute("NOSTR_BRIDGE_BLUESKY_OAUTH_CALLBACK_URL", cfg.Bluesky.OAuthCallbackURL, "/oauth/bluesky/callback"); err != nil {
			return Config{}, err
		}
		if err := requireOAuthRoute("NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_ID", cfg.Bluesky.OAuthClientID, "/oauth/bluesky/client-metadata.json"); err != nil {
			return Config{}, err
		}
		if !sameOrigin(cfg.Bluesky.OAuthCallbackURL, cfg.Bluesky.OAuthClientID) {
			return Config{}, fmt.Errorf("NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_ID must use the OAuth callback public origin")
		}
	}
	if cfg.Mastodon.Enabled() {
		if err := requireSettings([]setting{
			{"NOSTR_BRIDGE_MASTODON_ACCOUNT", cfg.Mastodon.Account},
			{"NOSTR_BRIDGE_MASTODON_OAUTH_CALLBACK_URL", cfg.Mastodon.OAuthCallbackURL},
			{"NOSTR_BRIDGE_MASTODON_OAUTH_CLIENT_ID", cfg.Mastodon.OAuthClientID},
			{"NOSTR_BRIDGE_MASTODON_OAUTH_CLIENT_SECRET", cfg.Mastodon.OAuthClientSecret},
			{"NOSTR_BRIDGE_MASTODON_OAUTH_ENCRYPTION_KEY", cfg.Mastodon.OAuthEncryptionKey},
		}); err != nil {
			return Config{}, err
		}
		if err := requireOAuthRoute("NOSTR_BRIDGE_MASTODON_OAUTH_CALLBACK_URL", cfg.Mastodon.OAuthCallbackURL, "/oauth/mastodon/callback"); err != nil {
			return Config{}, err
		}
	}
	if !validEndpoint(cfg.Shared.RelayURL, "ws", "wss") {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_RELAY_URL must be an absolute ws/wss URL")
	}
	if !validEndpoint(cfg.Shared.RelayManagementURL, "http", "https") {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_RELAY_MANAGEMENT_URL must be an absolute http/https URL")
	}
	if !validEndpoint(cfg.Shared.RelayCanonicalURL, "http", "https") {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_RELAY_CANONICAL_URL must be an absolute http/https URL")
	}
	if _, err := nostr.SecretKeyFromHex(cfg.Shared.RelayAdminPrivateKey); err != nil {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY must be valid secret hex")
	}
	seed, err := base64.StdEncoding.DecodeString(cfg.Shared.MasterSeed)
	if err != nil || len(seed) != 32 {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_MASTER_SEED must be base64 encoding of exactly 32 bytes")
	}
	if cfg.Shared.OutboxLimit <= 0 {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_OUTBOX_LIMIT must be positive")
	}
	if cfg.Shared.OutboxPollInterval <= 0 {
		return Config{}, fmt.Errorf("NOSTR_BRIDGE_OUTBOX_POLL_INTERVAL must be positive")
	}
	return cfg, nil
}

func requireOAuthRoute(name, raw, route string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != route {
		return fmt.Errorf("%s must be an absolute HTTPS URL with path %s", name, route)
	}
	return nil
}

func sameOrigin(a, b string) bool {
	x, errX := url.Parse(a)
	y, errY := url.Parse(b)
	return errX == nil && errY == nil && strings.EqualFold(x.Scheme, y.Scheme) && strings.EqualFold(x.Host, y.Host)
}

type setting struct{ name, value string }

func requireSettings(settings []setting) error {
	for _, setting := range settings {
		if strings.TrimSpace(setting.value) == "" {
			return fmt.Errorf("%s must not be empty", setting.name)
		}
	}
	return nil
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
