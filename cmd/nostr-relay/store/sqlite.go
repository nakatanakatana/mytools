package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"time"

	"fiatjaf.com/nostr"
	schema "github.com/nakatanakatana/mytools/cmd/nostr-relay/store/sql"
	storesqlc "github.com/nakatanakatana/mytools/cmd/nostr-relay/store/sqlc"
	storesqletch "github.com/nakatanakatana/mytools/cmd/nostr-relay/store/sqletch"
	_ "modernc.org/sqlite"
)

const eventTagsBackfilledMetaKey = "event_tags_backfilled"

type SQLiteStore struct {
	writeDB      *sql.DB
	readDB       *sql.DB
	writeQueries *storesqlc.Queries
	readQueries  *storesqlc.Queries
	eventQueries *storesqletch.Queries
}

var _ Store = (*SQLiteStore)(nil)

func sqliteDSN(databasePath string, readOnly bool) string {
	params := url.Values{}
	if readOnly {
		params.Add("mode", "ro")
		params.Add("_pragma", "busy_timeout(5000)")
		params.Add("_pragma", "query_only(ON)")
	} else {
		params.Add("_pragma", "journal_mode(WAL)")
		params.Add("_pragma", "foreign_keys(ON)")
		params.Add("_pragma", "busy_timeout(5000)")
		params.Add("_pragma", "synchronous(NORMAL)")
	}
	u := url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(databasePath),
		RawQuery: params.Encode(),
	}
	return u.String()
}

func Open(databasePath string) (*SQLiteStore, error) {
	if err := schema.Migrate(context.Background(), databasePath, false); err != nil {
		return nil, fmt.Errorf("migrate relay database: %w", err)
	}

	writeDB, err := sql.Open("sqlite", sqliteDSN(databasePath, false))
	if err != nil {
		return nil, err
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)
	if err := writeDB.Ping(); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("initialize relay write database: %w", err)
	}

	writeQueries := storesqlc.New(writeDB)
	if err := backfillEventTags(context.Background(), writeDB, writeQueries); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("backfill event tags: %w", err)
	}

	readDB, err := sql.Open("sqlite", sqliteDSN(databasePath, true))
	if err != nil {
		_ = writeDB.Close()
		return nil, err
	}
	poolSize := max(4, runtime.NumCPU())
	readDB.SetMaxOpenConns(poolSize)
	readDB.SetMaxIdleConns(poolSize)
	if err := readDB.Ping(); err != nil {
		_ = readDB.Close()
		_ = writeDB.Close()
		return nil, fmt.Errorf("initialize relay read database: %w", err)
	}

	return &SQLiteStore{
		writeDB:      writeDB,
		readDB:       readDB,
		writeQueries: writeQueries,
		readQueries:  storesqlc.New(readDB),
		eventQueries: storesqletch.New(readDB),
	}, nil
}

func (s *SQLiteStore) Close() error {
	return errors.Join(s.readDB.Close(), s.writeDB.Close())
}

func indexableTags(tags nostr.Tags) [][2]string {
	var result [][2]string
	for _, tag := range tags {
		if len(tag) < 2 || len(tag[0]) != 1 {
			continue
		}
		c := tag[0][0]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			result = append(result, [2]string{tag[0], tag[1]})
		}
	}
	return result
}

func backfillEventTags(ctx context.Context, writeDB *sql.DB, writeQueries *storesqlc.Queries) error {
	val, err := writeQueries.MetaValue(ctx, eventTagsBackfilledMetaKey)
	if err == nil && val == "1" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	q := writeQueries.WithTx(tx)
	rows, err := q.ListEventsForTagBackfill(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		var event nostr.Event
		if err := json.Unmarshal([]byte(row.EventJson), &event); err != nil {
			return fmt.Errorf("decode stored event %s: %w", row.ID, err)
		}
		for _, tag := range indexableTags(event.Tags) {
			if err := q.InsertEventTag(ctx, storesqlc.InsertEventTagParams{
				EventID:  row.ID,
				TagName:  tag[0],
				TagValue: tag[1],
			}); err != nil {
				return err
			}
		}
	}
	if err := q.SetMetaValue(ctx, storesqlc.SetMetaValueParams{
		Key:   eventTagsBackfilledMetaKey,
		Value: "1",
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) SaveEvent(ctx context.Context, event nostr.Event) error {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := saveEventTx(ctx, s.writeQueries.WithTx(tx), event); err != nil {
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
	eventID := event.ID.Hex()
	if err := queries.InsertEvent(ctx, storesqlc.InsertEventParams{
		ID: eventID, Pubkey: event.PubKey.Hex(), CreatedAt: int64(event.CreatedAt),
		Kind: int64(event.Kind), EventJson: string(encoded),
	}); err != nil {
		return err
	}
	for _, tag := range indexableTags(event.Tags) {
		if err := queries.InsertEventTag(ctx, storesqlc.InsertEventTagParams{
			EventID:  eventID,
			TagName:  tag[0],
			TagValue: tag[1],
		}); err != nil {
			return err
		}
	}
	return nil
}

// SaveEventAndApplyDeletion persists an event and applies its owned kind-5
// targets in one transaction, so an error cannot expose a partial deletion.
func (s *SQLiteStore) SaveEventAndApplyDeletion(ctx context.Context, event nostr.Event) error {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.writeQueries.WithTx(tx)
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

func (s *SQLiteStore) DeleteEvent(ctx context.Context, id nostr.ID) error {
	return s.writeQueries.DeleteEventByID(ctx, id.Hex())
}

func (s *SQLiteStore) Event(ctx context.Context, id nostr.ID) (nostr.Event, error) {
	encoded, err := s.readQueries.EventByID(ctx, id.Hex())
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
	return s.writeQueries.AllowPublisher(ctx, storesqlc.AllowPublisherParams{
		Pubkey: publisher.PubKey.Hex(), Reason: publisher.Reason, CreatedAt: publisher.CreatedAt.Unix(),
	})
}

func (s *SQLiteStore) UnallowPublisher(ctx context.Context, pubkey nostr.PubKey) error {
	return s.writeQueries.UnallowPublisher(ctx, pubkey.Hex())
}

func (s *SQLiteStore) PublisherAllowed(ctx context.Context, pubkey nostr.PubKey) (bool, error) {
	return s.readQueries.PublisherAllowed(ctx, pubkey.Hex())
}

func (s *SQLiteStore) ListPublishers(ctx context.Context) ([]Publisher, error) {
	rows, err := s.readQueries.ListPublishers(ctx)
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
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if retention <= 0 {
		retention = 60 * time.Second
	}
	queries := s.writeQueries.WithTx(tx)
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
