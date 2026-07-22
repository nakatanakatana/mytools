package schema

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrateCreatesBridgeSchema(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "bridge.db")
	if err := Migrate(context.Background(), databasePath, false); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open bridge database: %v", err)
	}
	defer func() { _ = db.Close() }()

	tables := []string{
		"bridge_events",
		"publisher_registrations",
		"publisher_purges",
		"sync_targets",
		"outbox_sequences",
		"outbox",
		"sync_cursors",
		"oauth_sessions",
		"oauth_tokens",
		"source_operations",
	}
	for _, table := range tables {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("query table %q: %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %q count = %d, want 1", table, count)
		}
	}
	var cursorType string
	if err := db.QueryRow(`SELECT type FROM pragma_table_info('sync_cursors') WHERE name='value'`).Scan(&cursorType); err != nil {
		t.Fatalf("query cursor value type: %v", err)
	}
	if cursorType != "TEXT" {
		t.Errorf("sync cursor value type = %q, want TEXT", cursorType)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "bridge.db")
	ctx := context.Background()
	if err := Migrate(ctx, databasePath, false); err != nil {
		t.Fatalf("initial Migrate() error = %v", err)
	}
	if err := Migrate(ctx, databasePath, true); err != nil {
		t.Fatalf("dry-run Migrate() error = %v", err)
	}
}

func TestMigratePreservesLegacyOAuthToken(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "bridge.db")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open legacy bridge database: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE oauth_tokens (
		provider TEXT NOT NULL,
		source_account TEXT NOT NULL,
		account_did TEXT NOT NULL,
		encrypted_payload BLOB NOT NULL,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY(provider, source_account, account_did)
	)`); err != nil {
		_ = db.Close()
		t.Fatalf("create legacy oauth_tokens: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO oauth_tokens(provider, source_account, account_did, encrypted_payload, updated_at) VALUES(?, ?, ?, ?, ?)`, "bluesky", "owner", "did:plc:alice", []byte("encrypted"), 17); err != nil {
		_ = db.Close()
		t.Fatalf("insert legacy OAuth token: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy bridge database: %v", err)
	}

	if err := Migrate(context.Background(), databasePath, false); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	db, err = sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("reopen bridge database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var payload []byte
	var updatedAt, lastRefreshAt, reauthRequired int64
	var lastRefreshErrorClass string
	if err := db.QueryRow(`SELECT encrypted_payload, updated_at, last_refresh_at, reauth_required, last_refresh_error_class FROM oauth_tokens WHERE provider=? AND source_account=? AND account_did=?`, "bluesky", "owner", "did:plc:alice").Scan(&payload, &updatedAt, &lastRefreshAt, &reauthRequired, &lastRefreshErrorClass); err != nil {
		t.Fatalf("query migrated OAuth token: %v", err)
	}
	if string(payload) != "encrypted" || updatedAt != 17 || lastRefreshAt != 0 || reauthRequired != 0 || lastRefreshErrorClass != "" {
		t.Fatalf("migrated OAuth token = payload %q, updatedAt %d, lastRefreshAt %d, reauthRequired %d, lastRefreshErrorClass %q", payload, updatedAt, lastRefreshAt, reauthRequired, lastRefreshErrorClass)
	}
}

func TestMigrateRestoresMissingOutboxIndex(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "bridge.db")
	ctx := context.Background()
	if err := Migrate(ctx, databasePath, false); err != nil {
		t.Fatalf("initial Migrate() error = %v", err)
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open bridge database: %v", err)
	}
	if _, err := db.Exec(`DROP INDEX outbox_claim_idx`); err != nil {
		_ = db.Close()
		t.Fatalf("drop outbox index: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close bridge database: %v", err)
	}

	if err := Migrate(ctx, databasePath, false); err != nil {
		t.Fatalf("repair Migrate() error = %v", err)
	}

	db, err = sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("reopen bridge database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'outbox_claim_idx'`).Scan(&count); err != nil {
		t.Fatalf("query outbox index: %v", err)
	}
	if count != 1 {
		t.Errorf("outbox_claim_idx count = %d, want 1", count)
	}
}
