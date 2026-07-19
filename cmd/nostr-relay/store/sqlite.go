package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"time"

	"fiatjaf.com/nostr"
	schema "github.com/nakatanakatana/mytools/cmd/nostr-relay/store/sql"
	storesqlc "github.com/nakatanakatana/mytools/cmd/nostr-relay/store/sqlc"
	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db      *sql.DB
	queries *storesqlc.Queries
}

var _ Store = (*SQLiteStore)(nil)

func Open(databasePath string) (*SQLiteStore, error) {
	if err := schema.Migrate(context.Background(), databasePath, false); err != nil {
		return nil, fmt.Errorf("migrate relay database: %w", err)
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialize relay database: %w", err)
		}
	}
	return &SQLiteStore{db: db, queries: storesqlc.New(db)}, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) SaveEvent(ctx context.Context, event nostr.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := saveEventTx(ctx, s.queries.WithTx(tx), event); err != nil {
		return err
	}
	return tx.Commit()
}

func saveEventTx(ctx context.Context, queries *storesqlc.Queries, event nostr.Event) error {
	tombstoned, err := queries.EventTombstoned(ctx, storesqlc.EventTombstonedParams{
		OwnerPubkey: event.PubKey.Hex(), TargetEventID: event.ID.Hex(),
	})
	if err != nil {
		return err
	}
	if tombstoned {
		return nil
	}
	if event.Kind.IsReplaceable() || event.Kind.IsAddressable() {
		rows, err := queries.EventsByPubKeyAndKind(ctx, storesqlc.EventsByPubKeyAndKindParams{
			Pubkey: event.PubKey.Hex(), Kind: int64(event.Kind),
		})
		if err != nil {
			return err
		}
		var deleteIDs []string
		storeIncoming := true
		for _, row := range rows {
			var previous nostr.Event
			if err := json.Unmarshal([]byte(row.EventJson), &previous); err != nil {
				return fmt.Errorf("decode stored event: %w", err)
			}
			if event.Kind.IsAddressable() && previous.Tags.GetD() != event.Tags.GetD() {
				continue
			}
			if nostr.IsOlder(previous, event) {
				deleteIDs = append(deleteIDs, row.ID)
			} else {
				storeIncoming = false
			}
		}
		if !storeIncoming {
			return nil
		}
		for _, id := range deleteIDs {
			if err := queries.DeleteEventByID(ctx, id); err != nil {
				return err
			}
		}
	}

	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return queries.InsertEvent(ctx, storesqlc.InsertEventParams{
		ID: event.ID.Hex(), Pubkey: event.PubKey.Hex(), CreatedAt: int64(event.CreatedAt),
		Kind: int64(event.Kind), EventJson: string(encoded),
	})
}

// SaveEventAndApplyDeletion persists an event and applies its owned kind-5
// targets in one transaction, so an error cannot expose a partial deletion.
func (s *SQLiteStore) SaveEventAndApplyDeletion(ctx context.Context, event nostr.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	if err := saveEventTx(ctx, queries, event); err != nil {
		return err
	}
	if event.Kind == 5 {
		for _, tag := range event.Tags {
			if len(tag) < 2 || tag[0] != "e" {
				continue
			}
			id, err := nostr.IDFromHex(tag[1])
			if err != nil {
				continue
			}
			// A kind-5 event is an owner's durable declaration that its target ID
			// must not be accepted, even when the target has not arrived yet.
			// Keying by the deleting owner preserves a foreign owner's event with
			// the same referenced ID.
			if err := queries.InsertDeletionTombstone(ctx, storesqlc.InsertDeletionTombstoneParams{
				OwnerPubkey: event.PubKey.Hex(), TargetEventID: id.Hex(),
				DeletionEventID: event.ID.Hex(), CreatedAt: int64(event.CreatedAt),
			}); err != nil {
				return err
			}
			if err := queries.DeleteOwnedEvent(ctx, storesqlc.DeleteOwnedEventParams{ID: id.Hex(), Pubkey: event.PubKey.Hex()}); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) QueryEvents(ctx context.Context, filter nostr.Filter) (iter.Seq[nostr.Event], error) {
	if filter.LimitZero {
		return func(func(nostr.Event) bool) {}, nil
	}
	idsJSON, err := json.Marshal(filter.IDs)
	if err != nil {
		return nil, err
	}
	authorsJSON, err := json.Marshal(filter.Authors)
	if err != nil {
		return nil, err
	}
	kindsJSON, err := json.Marshal(filter.Kinds)
	if err != nil {
		return nil, err
	}
	tagsJSON, err := json.Marshal(filter.Tags)
	if err != nil {
		return nil, err
	}
	encodedEvents, err := s.queries.AllEvents(ctx, storesqlc.AllEventsParams{
		HasIds: filter.IDs != nil, IdsJson: string(idsJSON),
		HasAuthors: filter.Authors != nil, AuthorsJson: string(authorsJSON),
		HasKinds: filter.Kinds != nil, KindsJson: string(kindsJSON),
		HasSince: filter.Since != 0, Since: int64(filter.Since),
		HasUntil: filter.Until != 0, Until: int64(filter.Until),
		HasTags: len(filter.Tags) != 0, TagsJson: string(tagsJSON), ResultLimit: filter.Limit,
	})
	if err != nil {
		return nil, err
	}
	events := make([]nostr.Event, 0, len(encodedEvents))
	for _, encoded := range encodedEvents {
		var event nostr.Event
		if err := json.Unmarshal([]byte(encoded), &event); err != nil {
			return nil, fmt.Errorf("decode stored event: %w", err)
		}
		events = append(events, event)
	}
	return func(yield func(nostr.Event) bool) {
		for _, event := range events {
			if !yield(event) {
				return
			}
		}
	}, nil
}

func (s *SQLiteStore) DeleteEvent(ctx context.Context, id nostr.ID) error {
	return s.queries.DeleteEventByID(ctx, id.Hex())
}

func (s *SQLiteStore) Event(ctx context.Context, id nostr.ID) (nostr.Event, error) {
	encoded, err := s.queries.EventByID(ctx, id.Hex())
	if err != nil {
		return nostr.Event{}, err
	}
	var event nostr.Event
	if err := json.Unmarshal([]byte(encoded), &event); err != nil {
		return nostr.Event{}, fmt.Errorf("decode stored event: %w", err)
	}
	return event, nil
}

func (s *SQLiteStore) AllowPublisher(ctx context.Context, publisher Publisher) error {
	return s.queries.AllowPublisher(ctx, storesqlc.AllowPublisherParams{
		Pubkey: publisher.PubKey.Hex(), Reason: publisher.Reason, CreatedAt: publisher.CreatedAt.Unix(),
	})
}

func (s *SQLiteStore) UnallowPublisher(ctx context.Context, pubkey nostr.PubKey) error {
	return s.queries.UnallowPublisher(ctx, pubkey.Hex())
}

func (s *SQLiteStore) PublisherAllowed(ctx context.Context, pubkey nostr.PubKey) (bool, error) {
	return s.queries.PublisherAllowed(ctx, pubkey.Hex())
}

func (s *SQLiteStore) ListPublishers(ctx context.Context) ([]Publisher, error) {
	rows, err := s.queries.ListPublishers(ctx)
	if err != nil {
		return nil, err
	}
	publishers := make([]Publisher, 0, len(rows))
	for _, row := range rows {
		pubkey, err := nostr.PubKeyFromHex(row.Pubkey)
		if err != nil {
			return nil, fmt.Errorf("decode publisher pubkey: %w", err)
		}
		publishers = append(publishers, Publisher{PubKey: pubkey, Reason: row.Reason, CreatedAt: time.Unix(row.CreatedAt, 0).UTC()})
	}
	return publishers, nil
}

// ConsumeNIP98Event atomically records a management authorization event.
// A duplicate remains rejected across processes and restarts.
func (s *SQLiteStore) ConsumeNIP98Event(ctx context.Context, id nostr.ID, now time.Time, retention time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if retention <= 0 {
		retention = 60 * time.Second
	}
	queries := s.queries.WithTx(tx)
	if err := queries.DeleteExpiredNIP98Events(ctx, now.Add(-retention).Unix()); err != nil {
		return err
	}
	result, err := queries.ConsumeNIP98Event(ctx, storesqlc.ConsumeNIP98EventParams{EventID: id.Hex(), SeenAt: now.Unix()})
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted != 1 {
		return fmt.Errorf("NIP-98 authorization event already consumed")
	}
	return tx.Commit()
}
