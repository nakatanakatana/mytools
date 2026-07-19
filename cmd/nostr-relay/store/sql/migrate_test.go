package schema

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrateCreatesRelaySchema(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "relay.db")
	if err := Migrate(context.Background(), databasePath, false); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open relay database: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, table := range []string{"events", "publisher_allowlist", "deletion_tombstones", "nip98_seen_events"} {
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
	databasePath := filepath.Join(t.TempDir(), "relay.db")
	ctx := context.Background()
	if err := Migrate(ctx, databasePath, false); err != nil {
		t.Fatalf("initial Migrate() error = %v", err)
	}
	if err := Migrate(ctx, databasePath, true); err != nil {
		t.Fatalf("dry-run Migrate() error = %v", err)
	}
}

func TestMigrateAddsMissingRelayIndex(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "relay.db")
	ctx := context.Background()
	if err := Migrate(ctx, databasePath, false); err != nil {
		t.Fatalf("initial Migrate() error = %v", err)
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open relay database: %v", err)
	}
	if _, err := db.Exec(`DROP INDEX events_created_at_idx`); err != nil {
		_ = db.Close()
		t.Fatalf("drop relay index: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close relay database: %v", err)
	}

	if err := Migrate(ctx, databasePath, false); err != nil {
		t.Fatalf("repair Migrate() error = %v", err)
	}

	db, err = sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("reopen relay database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'events_created_at_idx'`).Scan(&count); err != nil {
		t.Fatalf("query relay index: %v", err)
	}
	if count != 1 {
		t.Errorf("events_created_at_idx count = %d, want 1", count)
	}
}
