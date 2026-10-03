package main

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nakatanakatana/mytools/internal/webpush"
)

func validateConfigVariableCatalog(configType reflect.Type, catalog []configVariable) error {
	tagNames := make(map[string]int)
	var collect func(reflect.Type)
	collect = func(current reflect.Type) {
		for field := range current.Fields() {
			field := field
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
	if err := validateConfigVariableCatalog(reflect.TypeFor[Config](), configVariables); err != nil {
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
	err := validateConfigVariableCatalog(reflect.TypeFor[syntheticConfig](), catalog)
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
		if variable.removed || variable.documentation == documentUndocumented {
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

func readmeDocumentsDefault(contents, name, defaultValue string) bool {
	for line := range strings.SplitSeq(contents, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) == 5 &&
			strings.TrimSpace(cells[1]) == "`"+name+"`" &&
			strings.TrimSpace(cells[3]) == "`"+defaultValue+"`" {
			return true
		}
	}
	return false
}

func deploymentDocumentsEnvValue(contents, name, value string) bool {
	for line := range strings.SplitSeq(contents, "\n") {
		entry := strings.TrimSpace(line)
		if !strings.HasPrefix(entry, "- {") || !strings.HasSuffix(entry, "}") {
			continue
		}
		fields := strings.Split(strings.TrimSuffix(strings.TrimPrefix(entry, "- {"), "}"), ",")
		if len(fields) != 2 {
			continue
		}
		nameField := strings.SplitN(fields[0], ":", 2)
		valueField := strings.SplitN(fields[1], ":", 2)
		if len(nameField) == 2 && len(valueField) == 2 &&
			strings.TrimSpace(nameField[0]) == "name" &&
			strings.TrimSpace(nameField[1]) == name &&
			strings.TrimSpace(valueField[0]) == "value" &&
			strings.TrimSpace(valueField[1]) == value {
			return true
		}
	}
	return false
}

func TestConfigVariablesAreDocumented(t *testing.T) {
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

	for _, variable := range []struct {
		name, defaultValue string
	}{
		{"NOSTR_BRIDGE_BLUESKY_OAUTH_REFRESH_PERIOD", "720h"},
		{"NOSTR_BRIDGE_BLUESKY_OAUTH_REFRESH_CHECK_INTERVAL", "24h"},
	} {
		if !readmeDocumentsDefault(readme, variable.name, variable.defaultValue) {
			t.Errorf("README does not document %s with default %s", variable.name, variable.defaultValue)
		}
		if !deploymentDocumentsEnvValue(deployment, variable.name, variable.defaultValue) {
			t.Errorf("deployment does not document %s with default %s", variable.name, variable.defaultValue)
		}
	}
}

func TestConfigDocumentationRejectsLongerOrUnassociatedDefaults(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(contents)
	}

	for _, tc := range []struct {
		name, contents, variable, exactDefault, longerDefault, unrelatedEntry string
		matches                                                               func(string, string, string) bool
	}{
		{
			name: "README refresh period", contents: read("README.md"),
			variable: "NOSTR_BRIDGE_BLUESKY_OAUTH_REFRESH_PERIOD", exactDefault: "720h", longerDefault: "1720h",
			unrelatedEntry: "\n| `OTHER_VARIABLE` | Unrelated | `720h` |\n", matches: readmeDocumentsDefault,
		},
		{
			name: "README check interval", contents: read("README.md"),
			variable: "NOSTR_BRIDGE_BLUESKY_OAUTH_REFRESH_CHECK_INTERVAL", exactDefault: "24h", longerDefault: "124h",
			unrelatedEntry: "\n| `OTHER_VARIABLE` | Unrelated | `24h` |\n", matches: readmeDocumentsDefault,
		},
		{
			name: "deployment refresh period", contents: read("examples/kubernetes/deployment.yaml"),
			variable: "NOSTR_BRIDGE_BLUESKY_OAUTH_REFRESH_PERIOD", exactDefault: "720h", longerDefault: "1720h",
			unrelatedEntry: "\n- {name: OTHER_VARIABLE, value: 720h}\n", matches: deploymentDocumentsEnvValue,
		},
		{
			name: "deployment check interval", contents: read("examples/kubernetes/deployment.yaml"),
			variable: "NOSTR_BRIDGE_BLUESKY_OAUTH_REFRESH_CHECK_INTERVAL", exactDefault: "24h", longerDefault: "124h",
			unrelatedEntry: "\n- {name: OTHER_VARIABLE, value: 24h}\n", matches: deploymentDocumentsEnvValue,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.matches(tc.contents, tc.variable, tc.exactDefault) {
				t.Fatal("exact documented default was not matched")
			}
			mutated := strings.Replace(tc.contents, tc.exactDefault, tc.longerDefault, 1) + tc.unrelatedEntry
			if tc.matches(mutated, tc.variable, tc.exactDefault) {
				t.Fatalf("accepted %s for %s because %s occurred elsewhere", tc.longerDefault, tc.variable, tc.exactDefault)
			}
		})
	}
}

func setSharedEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NOSTR_BRIDGE_HOST", "127.0.0.1")
	t.Setenv("NOSTR_BRIDGE_PORT", "4321")
	t.Setenv("NOSTR_BRIDGE_UI_URL", "https://dashboard.example")
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

func TestLoadConfigDefaultsOAuthRefreshDurations(t *testing.T) {
	setSharedEnv(t)
	setBlueskyEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bluesky.OAuthRefreshPeriod != 720*time.Hour {
		t.Fatalf("OAuthRefreshPeriod = %s, want 720h", cfg.Bluesky.OAuthRefreshPeriod)
	}
	if cfg.Bluesky.OAuthRefreshCheckInterval != 24*time.Hour {
		t.Fatalf("OAuthRefreshCheckInterval = %s, want 24h", cfg.Bluesky.OAuthRefreshCheckInterval)
	}
}

func TestLoadConfigRejectsNonPositiveOAuthRefreshDurations(t *testing.T) {
	for _, variable := range []string{
		"NOSTR_BRIDGE_BLUESKY_OAUTH_REFRESH_PERIOD",
		"NOSTR_BRIDGE_BLUESKY_OAUTH_REFRESH_CHECK_INTERVAL",
	} {
		for _, value := range []string{"0s", "-1s"} {
			t.Run(variable+"/"+value, func(t *testing.T) {
				setSharedEnv(t)
				setBlueskyEnv(t)
				t.Setenv(variable, value)

				_, err := LoadConfig()
				if err == nil || !strings.Contains(err.Error(), variable) {
					t.Fatalf("LoadConfig() error = %v, want %s", err, variable)
				}
			})
		}
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

func TestLoadConfigRejectsInvalidUIURL(t *testing.T) {
	for _, value := range []string{"   ", "/private", "javascript:alert(1)", "https://user:pass@dashboard.example/", "https://dashboard.example/nostr-bridge"} {
		t.Run(value, func(t *testing.T) {
			setSharedEnv(t)
			setBlueskyEnv(t)
			t.Setenv("NOSTR_BRIDGE_UI_URL", value)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "NOSTR_BRIDGE_UI_URL") {
				t.Fatalf("accepted NOSTR_BRIDGE_UI_URL=%q: %v", value, err)
			}
		})
	}
}

func TestLoadConfigAllowsMissingUIURL(t *testing.T) {
	setSharedEnv(t)
	setBlueskyEnv(t)
	t.Setenv("NOSTR_BRIDGE_UI_URL", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want missing UI URL to be allowed", err)
	}
	if cfg.Shared.UIURL != "" {
		t.Fatalf("UIURL = %q, want empty", cfg.Shared.UIURL)
	}
}

func TestLoadConfigReadsUIURL(t *testing.T) {
	setSharedEnv(t)
	setBlueskyEnv(t)
	t.Setenv("NOSTR_BRIDGE_UI_URL", "https://dashboard.example/?source=oauth")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Shared.UIURL != "https://dashboard.example/?source=oauth" {
		t.Fatalf("UIURL = %q", cfg.Shared.UIURL)
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

func TestConfigNotificationDefaults(t *testing.T) {
	setSharedEnv(t)
	setBlueskyEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Notification.RemindInterval != 24*time.Hour {
		t.Fatalf("RemindInterval = %v, want 24h", cfg.Notification.RemindInterval)
	}
	if cfg.Notification.EvaluationInterval != 15*time.Second {
		t.Fatalf("EvaluationInterval = %v, want 15s", cfg.Notification.EvaluationInterval)
	}
	if cfg.Notification.VAPIDSubject != "mailto:admin@localhost" {
		t.Fatalf("VAPIDSubject = %q, want mailto:admin@localhost", cfg.Notification.VAPIDSubject)
	}
	if cfg.Notification.VAPIDPrivateKey != "" {
		t.Fatalf("VAPIDPrivateKey = %q, want empty", cfg.Notification.VAPIDPrivateKey)
	}
	if cfg.Notification.VAPIDPublicKey != "" {
		t.Fatalf("VAPIDPublicKey = %q, want empty", cfg.Notification.VAPIDPublicKey)
	}
}

func TestConfigNotificationCustomEnv(t *testing.T) {
	setSharedEnv(t)
	setBlueskyEnv(t)

	keys, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}

	t.Setenv("NOSTR_BRIDGE_VAPID_PRIVATE_KEY", keys.PrivateKey)
	t.Setenv("NOSTR_BRIDGE_VAPID_PUBLIC_KEY", keys.PublicKey)
	t.Setenv("NOSTR_BRIDGE_VAPID_SUBJECT", "mailto:alerts@example.com")
	t.Setenv("NOSTR_BRIDGE_NOTIFICATION_REMIND_INTERVAL", "12h")
	t.Setenv("NOSTR_BRIDGE_NOTIFICATION_EVALUATION_INTERVAL", "30s")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Notification.VAPIDPrivateKey != keys.PrivateKey {
		t.Fatalf("VAPIDPrivateKey = %q, want %q", cfg.Notification.VAPIDPrivateKey, keys.PrivateKey)
	}
	if cfg.Notification.VAPIDPublicKey != keys.PublicKey {
		t.Fatalf("VAPIDPublicKey = %q, want %q", cfg.Notification.VAPIDPublicKey, keys.PublicKey)
	}
	if cfg.Notification.VAPIDSubject != "mailto:alerts@example.com" {
		t.Fatalf("VAPIDSubject = %q, want mailto:alerts@example.com", cfg.Notification.VAPIDSubject)
	}
	if cfg.Notification.RemindInterval != 12*time.Hour {
		t.Fatalf("RemindInterval = %v, want 12h", cfg.Notification.RemindInterval)
	}
	if cfg.Notification.EvaluationInterval != 30*time.Second {
		t.Fatalf("EvaluationInterval = %v, want 30s", cfg.Notification.EvaluationInterval)
	}
}

func TestConfigNotificationRejectsInvalidDurations(t *testing.T) {
	for _, tc := range []struct {
		variable, field string
	}{
		{"NOSTR_BRIDGE_NOTIFICATION_REMIND_INTERVAL", "RemindInterval"},
		{"NOSTR_BRIDGE_NOTIFICATION_EVALUATION_INTERVAL", "EvaluationInterval"},
	} {
		for _, value := range []string{"invalid", "not-a-duration"} {
			t.Run(tc.variable+"/"+value, func(t *testing.T) {
				setSharedEnv(t)
				setBlueskyEnv(t)
				t.Setenv(tc.variable, value)

				_, err := LoadConfig()
				if err == nil {
					t.Fatalf("LoadConfig() expected error for %s=%s, got nil", tc.variable, value)
				}
			})
		}
		for _, value := range []string{"0s", "-1s"} {
			t.Run(tc.variable+"/"+value, func(t *testing.T) {
				setSharedEnv(t)
				setBlueskyEnv(t)
				t.Setenv(tc.variable, value)

				_, err := LoadConfig()
				if err == nil || !strings.Contains(err.Error(), tc.variable) {
					t.Fatalf("LoadConfig() error = %v, want error containing %s", err, tc.variable)
				}
			})
		}
	}
}

func TestConfigNotificationRejectsIntervalsBelowMinimum(t *testing.T) {
	for _, tc := range []struct {
		variable string
		value    string
	}{
		{"NOSTR_BRIDGE_NOTIFICATION_REMIND_INTERVAL", "59s"},
		{"NOSTR_BRIDGE_NOTIFICATION_EVALUATION_INTERVAL", "999ms"},
	} {
		t.Run(tc.variable, func(t *testing.T) {
			setSharedEnv(t)
			setBlueskyEnv(t)
			t.Setenv(tc.variable, tc.value)

			_, err := LoadConfig()
			if err == nil || !strings.Contains(err.Error(), tc.variable) {
				t.Fatalf("LoadConfig() error = %v, want error containing %s", err, tc.variable)
			}
		})
	}
}

func TestConfigNotificationValidatesVAPIDKeys(t *testing.T) {
	t.Run("private key only", func(t *testing.T) {
		setSharedEnv(t)
		setBlueskyEnv(t)
		t.Setenv("NOSTR_BRIDGE_VAPID_PRIVATE_KEY", "privkey")
		t.Setenv("NOSTR_BRIDGE_VAPID_PUBLIC_KEY", "")

		_, err := LoadConfig()
		if err == nil {
			t.Fatal("expected error when only private key is set")
		}
	})

	t.Run("public key only", func(t *testing.T) {
		setSharedEnv(t)
		setBlueskyEnv(t)
		t.Setenv("NOSTR_BRIDGE_VAPID_PRIVATE_KEY", "")
		t.Setenv("NOSTR_BRIDGE_VAPID_PUBLIC_KEY", "pubkey")

		_, err := LoadConfig()
		if err == nil {
			t.Fatal("expected error when only public key is set")
		}
	})

	t.Run("both keys present valid", func(t *testing.T) {
		setSharedEnv(t)
		setBlueskyEnv(t)
		keys, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			t.Fatalf("GenerateVAPIDKeys: %v", err)
		}
		t.Setenv("NOSTR_BRIDGE_VAPID_PRIVATE_KEY", keys.PrivateKey)
		t.Setenv("NOSTR_BRIDGE_VAPID_PUBLIC_KEY", keys.PublicKey)

		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Notification.VAPIDPrivateKey != keys.PrivateKey || cfg.Notification.VAPIDPublicKey != keys.PublicKey {
			t.Fatalf("keys not set correctly: %+v", cfg.Notification)
		}
	})

	t.Run("both keys present but invalid pair", func(t *testing.T) {
		setSharedEnv(t)
		setBlueskyEnv(t)
		t.Setenv("NOSTR_BRIDGE_VAPID_PRIVATE_KEY", "invalid-key")
		t.Setenv("NOSTR_BRIDGE_VAPID_PUBLIC_KEY", "invalid-key")

		_, err := LoadConfig()
		if err == nil {
			t.Fatal("expected error for invalid VAPID key pair, got nil")
		}
	})

	t.Run("subject invalid scheme", func(t *testing.T) {
		setSharedEnv(t)
		setBlueskyEnv(t)
		t.Setenv("NOSTR_BRIDGE_VAPID_SUBJECT", "invalid-subject")

		_, err := LoadConfig()
		if err == nil {
			t.Fatal("expected error for subject without mailto: or https:, got nil")
		}
	})

	t.Run("subject empty", func(t *testing.T) {
		setSharedEnv(t)
		setBlueskyEnv(t)
		t.Setenv("NOSTR_BRIDGE_VAPID_SUBJECT", "   ")

		_, err := LoadConfig()
		if err == nil {
			t.Fatal("expected error for empty subject, got nil")
		}
	})
}
