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
