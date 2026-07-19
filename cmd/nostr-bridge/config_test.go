package main

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func validateConfigVariableCatalog(configType reflect.Type, catalog []configVariable) error {
	tagNames := make(map[string]int)
	var collect func(reflect.Type)
	collect = func(current reflect.Type) {
		for i := 0; i < current.NumField(); i++ {
			field := current.Field(i)
			tag := field.Tag.Get("env")
			if tag != "" {
				name, _, _ := strings.Cut(tag, ",")
				tagNames[name]++
				continue
			}
			if field.Type.Kind() == reflect.Struct {
				collect(field.Type)
			}
		}
	}
	collect(configType)

	catalogNames := make(map[string]int)
	for _, variable := range catalog {
		if !variable.removed {
			catalogNames[variable.name]++
		}
	}
	var problems []string
	for name, count := range tagNames {
		if count > 1 {
			problems = append(problems, fmt.Sprintf("duplicate env tag %s", name))
		}
		if catalogNames[name] == 0 {
			problems = append(problems, "missing "+name)
		}
	}
	for name, count := range catalogNames {
		if count > 1 {
			problems = append(problems, fmt.Sprintf("duplicate catalog variable %s", name))
		}
		if tagNames[name] == 0 {
			problems = append(problems, "extra "+name)
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return fmt.Errorf("config variable catalog mismatch: %s", strings.Join(problems, "; "))
	}
	return nil
}

func TestConfigVariableCatalogMatchesEnvTags(t *testing.T) {
	if err := validateConfigVariableCatalog(reflect.TypeOf(Config{}), configVariables); err != nil {
		t.Fatal(err)
	}
}

func TestConfigVariableCatalogDetectsTagMismatch(t *testing.T) {
	type syntheticConfig struct {
		Present string `env:"PRESENT,required"`
		Nested  struct {
			Missing string `env:"MISSING"`
		}
	}
	catalog := []configVariable{{name: "PRESENT"}, {name: "EXTRA"}}
	err := validateConfigVariableCatalog(reflect.TypeOf(syntheticConfig{}), catalog)
	if err == nil || !strings.Contains(err.Error(), "missing MISSING") || !strings.Contains(err.Error(), "extra EXTRA") {
		t.Fatalf("err = %v", err)
	}
}

func TestDocumentedConfigurationMatchesConfig(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(contents)
	}
	readme := read("README.md")
	deployment := read("examples/kubernetes/deployment.yaml")

	for _, variable := range configVariables {
		if variable.removed {
			if strings.Contains(readme, "`"+variable.name+"`") || strings.Contains(deployment, "name: "+variable.name) {
				t.Errorf("documentation contains removed configuration variable %s", variable.name)
			}
			continue
		}
		if !strings.Contains(readme, "`"+variable.name+"`") {
			t.Errorf("README does not contain %s", variable.name)
		}
		if variable.documentation == documentInReadmeAndDeployment && !strings.Contains(deployment, "name: "+variable.name) {
			t.Errorf("deployment does not contain %s", variable.name)
		}
	}
}

func setSharedEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NOSTR_BRIDGE_HOST", "127.0.0.1")
	t.Setenv("NOSTR_BRIDGE_PORT", "4321")
	t.Setenv("NOSTR_BRIDGE_DATABASE_PATH", "/tmp/nostr-bridge.db")
	t.Setenv("NOSTR_BRIDGE_MASTER_SEED", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("NOSTR_BRIDGE_RELAY_URL", "wss://relay.example")
	t.Setenv("NOSTR_BRIDGE_RELAY_MANAGEMENT_URL", "https://relay.example/manage")
	t.Setenv("NOSTR_BRIDGE_RELAY_CANONICAL_URL", "https://relay.example/manage")
	t.Setenv("NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY", strings.Repeat("1", 64))
	t.Setenv("NOSTR_BRIDGE_OWNER_ID", "home")
}

func setBlueskyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NOSTR_BRIDGE_BLUESKY_ACCOUNT_DID", "did:plc:owner")
	t.Setenv("NOSTR_BRIDGE_BLUESKY_BASE_URL", "https://bsky.example")
	t.Setenv("NOSTR_BRIDGE_BLUESKY_JETSTREAM_URL", "wss://jetstream.example/subscribe")
	t.Setenv("NOSTR_BRIDGE_BLUESKY_LIST_URIS", "at://did:plc:one/app.bsky.graph.list/one,at://did:plc:two/app.bsky.graph.list/two")
	t.Setenv("NOSTR_BRIDGE_BLUESKY_BACKFILL_LIMIT", "25")
	t.Setenv("NOSTR_BRIDGE_BLUESKY_RECONCILE_INTERVAL", "15m")
	t.Setenv("NOSTR_BRIDGE_BLUESKY_OAUTH_CALLBACK_URL", "https://bridge.example/oauth/bluesky/callback")
	t.Setenv("NOSTR_BRIDGE_BLUESKY_OAUTH_AUTHORIZATION_SERVER_URL", "https://oauth.example")
	t.Setenv("NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_ID", "https://bridge.example/oauth/bluesky/client-metadata.json")
	t.Setenv("NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_SIGNING_KEY", "test-secret")
	t.Setenv("NOSTR_BRIDGE_BLUESKY_OAUTH_ENCRYPTION_KEY", "test-secret")
}

func TestLoadConfigRejectsOAuthRoutePathMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, env, value string
		setup            func(*testing.T)
	}{
		{"bluesky callback", "NOSTR_BRIDGE_BLUESKY_OAUTH_CALLBACK_URL", "https://bridge.example/oauth/callback", setBlueskyEnv},
		{"bluesky metadata", "NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_ID", "https://bridge.example/oauth/client-metadata.json", setBlueskyEnv},
		{"mastodon callback", "NOSTR_BRIDGE_MASTODON_OAUTH_CALLBACK_URL", "https://bridge.example/oauth/callback", setMastodonEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSharedEnv(t)
			tc.setup(t)
			t.Setenv(tc.env, tc.value)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), tc.env) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func setMastodonEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NOSTR_BRIDGE_MASTODON_BASE_URL", "https://social.example")
	t.Setenv("NOSTR_BRIDGE_MASTODON_ACCOUNT", "owner@social.example")
	t.Setenv("NOSTR_BRIDGE_MASTODON_LIST_IDS", "1,2")
	t.Setenv("NOSTR_BRIDGE_MASTODON_BACKFILL_LIMIT", "30")
	t.Setenv("NOSTR_BRIDGE_MASTODON_RECONCILE_INTERVAL", "20m")
	t.Setenv("NOSTR_BRIDGE_MASTODON_OAUTH_CALLBACK_URL", "https://bridge.example/oauth/mastodon/callback")
	t.Setenv("NOSTR_BRIDGE_MASTODON_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("NOSTR_BRIDGE_MASTODON_OAUTH_CLIENT_SECRET", "client-secret")
	t.Setenv("NOSTR_BRIDGE_MASTODON_OAUTH_ENCRYPTION_KEY", "encryption-key")
}

func TestLoadConfigEnablesBothProviders(t *testing.T) {
	setSharedEnv(t)
	setBlueskyEnv(t)
	setMastodonEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Bluesky.Enabled() || !cfg.Mastodon.Enabled() {
		t.Fatalf("providers = %#v", cfg)
	}
	if cfg.Shared.Host != "127.0.0.1" || cfg.Shared.Port != "4321" || cfg.Owner.ID != "home" || cfg.Owner.Name != "nostr-bridge" {
		t.Fatalf("shared or owner config = %#v", cfg)
	}
	if cfg.Bluesky.AccountDID != "did:plc:owner" || len(cfg.Bluesky.ListURIs) != 2 || cfg.Bluesky.BackfillLimit != 25 || cfg.Bluesky.ReconcileInterval != 15*time.Minute {
		t.Fatalf("Bluesky config = %#v", cfg.Bluesky)
	}
	if cfg.Mastodon.Account != "owner@social.example" || len(cfg.Mastodon.ListIDs) != 2 || cfg.Mastodon.BackfillLimit != 30 || cfg.Mastodon.ReconcileInterval != 20*time.Minute {
		t.Fatalf("Mastodon config = %#v", cfg.Mastodon)
	}
}

func TestLoadConfigEnablesOneProvider(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*testing.T)
		want func(Config) bool
	}{
		{"Bluesky", setBlueskyEnv, func(c Config) bool { return c.Bluesky.Enabled() && !c.Mastodon.Enabled() }},
		{"Mastodon", setMastodonEnv, func(c Config) bool { return !c.Bluesky.Enabled() && c.Mastodon.Enabled() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSharedEnv(t)
			tc.set(t)
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if !tc.want(cfg) {
				t.Fatalf("providers = %#v", cfg)
			}
		})
	}
}

func TestLoadConfigRejectsNoProvider(t *testing.T) {
	setSharedEnv(t)
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "at least one provider") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadConfigRejectsPartialProviderSettings(t *testing.T) {
	for _, provider := range []struct {
		name      string
		variables []string
		set       func(*testing.T)
	}{
		{"Bluesky", []string{
			"NOSTR_BRIDGE_BLUESKY_ACCOUNT_DID",
			"NOSTR_BRIDGE_BLUESKY_JETSTREAM_URL",
			"NOSTR_BRIDGE_BLUESKY_OAUTH_CALLBACK_URL",
			"NOSTR_BRIDGE_BLUESKY_OAUTH_AUTHORIZATION_SERVER_URL",
			"NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_ID",
			"NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_SIGNING_KEY",
			"NOSTR_BRIDGE_BLUESKY_OAUTH_ENCRYPTION_KEY",
		}, setBlueskyEnv},
		{"Mastodon", []string{
			"NOSTR_BRIDGE_MASTODON_ACCOUNT",
			"NOSTR_BRIDGE_MASTODON_OAUTH_CALLBACK_URL",
			"NOSTR_BRIDGE_MASTODON_OAUTH_CLIENT_ID",
			"NOSTR_BRIDGE_MASTODON_OAUTH_CLIENT_SECRET",
			"NOSTR_BRIDGE_MASTODON_OAUTH_ENCRYPTION_KEY",
		}, setMastodonEnv},
	} {
		for _, variable := range provider.variables {
			t.Run(provider.name+"/"+variable, func(t *testing.T) {
				setSharedEnv(t)
				provider.set(t)
				t.Setenv(variable, "")
				_, err := LoadConfig()
				if err == nil || !strings.Contains(err.Error(), variable) {
					t.Fatalf("err = %v", err)
				}
			})
		}
	}
}

func TestLoadConfigValidatesOwner(t *testing.T) {
	t.Run("ID required", func(t *testing.T) {
		setSharedEnv(t)
		setBlueskyEnv(t)
		t.Setenv("NOSTR_BRIDGE_OWNER_ID", "")
		if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "OWNER_ID") {
			t.Fatalf("err = %v", err)
		}
	})
	for _, picture := range []string{"http://example.com/picture.jpg", "/picture.jpg", "https://user@example.com/picture.jpg", "https://example.com/picture.jpg#fragment"} {
		t.Run(picture, func(t *testing.T) {
			setSharedEnv(t)
			setBlueskyEnv(t)
			t.Setenv("NOSTR_BRIDGE_OWNER_PICTURE", picture)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "OWNER_PICTURE") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	t.Run("HTTPS picture accepted", func(t *testing.T) {
		setSharedEnv(t)
		setBlueskyEnv(t)
		t.Setenv("NOSTR_BRIDGE_OWNER_PICTURE", "https://example.com/picture.jpg")
		if _, err := LoadConfig(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLoadConfigRejectsRemovedGenericAliases(t *testing.T) {
	for _, variable := range []string{
		"NOSTR_BRIDGE_ACCOUNT_DID",
		"NOSTR_BRIDGE_JETSTREAM_URL",
		"NOSTR_BRIDGE_LIST_URIS",
		"NOSTR_BRIDGE_BACKFILL_LIMIT",
		"NOSTR_BRIDGE_RECONCILE_INTERVAL",
		"NOSTR_BRIDGE_OAUTH_CALLBACK_URL",
		"NOSTR_BRIDGE_OAUTH_AUTHORIZATION_SERVER_URL",
		"NOSTR_BRIDGE_OAUTH_CLIENT_ID",
		"NOSTR_BRIDGE_OAUTH_CLIENT_SIGNING_KEY",
		"NOSTR_BRIDGE_OAUTH_ENCRYPTION_KEY",
	} {
		t.Run(variable, func(t *testing.T) {
			setSharedEnv(t)
			setBlueskyEnv(t)
			t.Setenv(variable, "legacy")
			_, err := LoadConfig()
			if err == nil || !strings.Contains(err.Error(), "removed configuration variable") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestLoadConfigRejectsInvalidSharedSettings(t *testing.T) {
	for _, tc := range []struct{ variable, value string }{
		{"NOSTR_BRIDGE_DATABASE_PATH", ""},
		{"NOSTR_BRIDGE_RELAY_URL", "https://relay.example"},
		{"NOSTR_BRIDGE_RELAY_MANAGEMENT_URL", "/relative"},
		{"NOSTR_BRIDGE_RELAY_CANONICAL_URL", "https://user@relay.example"},
		{"NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY", "invalid"},
		{"NOSTR_BRIDGE_MASTER_SEED", "c2VlZA=="},
	} {
		t.Run(tc.variable, func(t *testing.T) {
			setSharedEnv(t)
			setBlueskyEnv(t)
			t.Setenv(tc.variable, tc.value)
			if _, err := LoadConfig(); err == nil {
				t.Fatalf("accepted %s=%q", tc.variable, tc.value)
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
