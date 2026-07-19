package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"fiatjaf.com/nostr"
	schema "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store/sql"
	storesqlc "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store/sqlc"
	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db      *sql.DB
	queries *storesqlc.Queries
}

var _ OAuthStore = SQLiteStore{}
var _ OutboxStore = SQLiteStore{}

// Open opens a SQLite state database and applies the bridge schema.
func Open(ctx context.Context, databasePath string) (SQLiteStore, io.Closer, error) {
	if err := schema.Migrate(ctx, databasePath, false); err != nil {
		return SQLiteStore{}, nil, fmt.Errorf("migrate bridge SQLite schema: %w", err)
	}
	db, err := sql.Open("sqlite", databasePath+"?_pragma=busy_timeout%3d5000")
	if err != nil {
		return SQLiteStore{}, nil, fmt.Errorf("open SQLite database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return SQLiteStore{}, nil, fmt.Errorf("ping SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	queries := storesqlc.New(db)
	return SQLiteStore{db: db, queries: queries}, db, nil
}

func enqueueOutbox(ctx context.Context, queries *storesqlc.Queries, request OutboxRequest) error {
	if err := queries.AdvanceOutboxSequence(ctx, request.AggregateKey); err != nil {
		return fmt.Errorf("advance outbox sequence: %w", err)
	}
	if err := queries.EnqueueOutbox(ctx, storesqlc.EnqueueOutboxParams{
		AggregateKey: request.AggregateKey, Operation: string(request.Operation), Pubkey: request.PubKey,
		Payload: request.Payload, AvailableAt: request.AvailableAt.Unix(), SequenceAggregateKey: request.AggregateKey,
	}); err != nil {
		return fmt.Errorf("enqueue outbox item: %w", err)
	}
	return nil
}

func rejectPurged(ctx context.Context, queries *storesqlc.Queries, pubkey string) error {
	purged, err := queries.PublisherPurged(ctx, pubkey)
	if err != nil {
		return fmt.Errorf("check publisher purge: %w", err)
	}
	if purged {
		return ErrPurgePending
	}
	return nil
}

func upsertEventMapping(ctx context.Context, queries *storesqlc.Queries, mapping EventMapping) error {
	return queries.UpsertEventMapping(ctx, storesqlc.UpsertEventMappingParams{
		Provider: mapping.Source.Scope.Provider, SourceAccount: mapping.Source.Scope.Account, SourceUri: mapping.Source.URI, NostrEventID: mapping.NostrEventID, SourceKind: mapping.SourceKind,
		AuthorPubkey: mapping.AuthorPubKey, UpdatedAt: mapping.UpdatedAt,
	})
}

func eventMapping(row storesqlc.BridgeEvent) EventMapping {
	return EventMapping{Source: SourceRef{Scope: SourceScope{Provider: row.Provider, Account: row.SourceAccount}, URI: row.SourceUri}, NostrEventID: row.NostrEventID, SourceKind: row.SourceKind, AuthorPubKey: row.AuthorPubkey, UpdatedAt: row.UpdatedAt}
}

func outboxItem(row storesqlc.ClaimOutboxRow) OutboxItem {
	return OutboxItem{ID: row.ID, AggregateKey: row.AggregateKey, Sequence: row.Sequence, Operation: OutboxOperation(row.Operation), PubKey: row.Pubkey, Payload: row.Payload, Attempts: int(row.Attempts), AvailableAt: time.Unix(row.AvailableAt, 0), LastError: row.LastError}
}

func oauthSession(row storesqlc.OauthSession) OAuthSession {
	return OAuthSession{State: row.State, EncryptedPayload: row.EncryptedPayload, ExpiresAt: row.ExpiresAt}
}

func oauthToken(row storesqlc.OauthToken) OAuthToken {
	return OAuthToken{AccountDID: row.AccountDid, EncryptedPayload: row.EncryptedPayload, UpdatedAt: row.UpdatedAt}
}

func eventIDParams(ref SourceRef) storesqlc.EventIDBySourceURIParams {
	return storesqlc.EventIDBySourceURIParams{Provider: ref.Scope.Provider, SourceAccount: ref.Scope.Account, SourceUri: ref.URI}
}
func authorParams(ref SourceRef) storesqlc.EventAuthorBySourceURIParams {
	return storesqlc.EventAuthorBySourceURIParams{Provider: ref.Scope.Provider, SourceAccount: ref.Scope.Account, SourceUri: ref.URI}
}
func mappingParams(ref SourceRef) storesqlc.EventMappingBySourceURIParams {
	return storesqlc.EventMappingBySourceURIParams{Provider: ref.Scope.Provider, SourceAccount: ref.Scope.Account, SourceUri: ref.URI}
}
func deleteMappingParams(ref SourceRef) storesqlc.DeleteEventMappingParams {
	return storesqlc.DeleteEventMappingParams{Provider: ref.Scope.Provider, SourceAccount: ref.Scope.Account, SourceUri: ref.URI}
}
func operationParams(ref SourceRef) storesqlc.SourceOperationBySourceURIParams {
	return storesqlc.SourceOperationBySourceURIParams{Provider: ref.Scope.Provider, SourceAccount: ref.Scope.Account, SourceUri: ref.URI}
}
func deleteOperationParams(ref SourceRef) storesqlc.DeleteSourceOperationParams {
	return storesqlc.DeleteSourceOperationParams{Provider: ref.Scope.Provider, SourceAccount: ref.Scope.Account, SourceUri: ref.URI}
}
func targetScopeParams(scope SourceScope) storesqlc.ReplaceSyncTargetsDeleteParams {
	return storesqlc.ReplaceSyncTargetsDeleteParams{Provider: scope.Provider, SourceAccount: scope.Account}
}
func saveSourceOperation(ctx context.Context, q *storesqlc.Queries, ref SourceRef, identity string) error {
	return q.SaveSourceOperation(ctx, storesqlc.SaveSourceOperationParams{Provider: ref.Scope.Provider, SourceAccount: ref.Scope.Account, SourceUri: ref.URI, Identity: identity})
}
func saveCursor(ctx context.Context, q *storesqlc.Queries, scope SourceScope, cursor *CursorUpdate) error {
	return q.SaveCursor(ctx, storesqlc.SaveCursorParams{Provider: scope.Provider, SourceAccount: scope.Account, Name: cursor.Name, Value: cursor.Value})
}

func validatePublishRequest(request OutboxRequest) error {
	if request.Operation != OutboxPublishEvent {
		return ErrInvalidOutboxPayload
	}
	pubkey, err := nostr.PubKeyFromHex(request.PubKey)
	if err != nil || pubkey.Hex() != request.PubKey || request.AggregateKey != request.PubKey {
		return ErrAuthorMismatch
	}
	var event nostr.Event
	if err := json.Unmarshal([]byte(request.Payload), &event); err != nil || !event.CheckID() || !event.VerifySignature() {
		return ErrInvalidOutboxPayload
	}
	if event.PubKey.Hex() != request.PubKey {
		return ErrAuthorMismatch
	}
	return nil
}

func validateMappingEventID(mapping EventMapping, request OutboxRequest) error {
	var event nostr.Event
	if err := json.Unmarshal([]byte(request.Payload), &event); err != nil || mapping.NostrEventID != event.ID.Hex() {
		return ErrInvalidOutboxPayload
	}
	return nil
}

func (s SQLiteStore) EnqueueOutbox(ctx context.Context, request OutboxRequest) error {
	if request.Operation == OutboxAllowPublisher || request.Operation == OutboxPublishEvent || request.Operation == OutboxUnallowPublisher {
		pubkey, err := nostr.PubKeyFromHex(request.PubKey)
		if err != nil || pubkey.Hex() != request.PubKey || request.AggregateKey != request.PubKey {
			return ErrAuthorMismatch
		}
	}
	if request.Operation == OutboxPublishEvent {
		var event nostr.Event
		if err := json.Unmarshal([]byte(request.Payload), &event); err != nil || !event.CheckID() || !event.VerifySignature() {
			return ErrInvalidOutboxPayload
		}
		if event.PubKey.Hex() != request.PubKey {
			return ErrAuthorMismatch
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin enqueue outbox transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	if request.Operation != OutboxUnallowPublisher {
		if err := rejectPurged(ctx, queries, request.PubKey); err != nil {
			return err
		}
	}
	if err := enqueueOutbox(ctx, queries, request); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit enqueue outbox transaction: %w", err)
	}
	return nil
}

func (s SQLiteStore) SaveEventAndEnqueue(ctx context.Context, event EventMapping, request OutboxRequest) error {
	if request.PubKey != event.AuthorPubKey {
		return ErrAuthorMismatch
	}
	if err := validatePublishRequest(request); err != nil {
		return err
	}
	if err := validateMappingEventID(event, request); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save event and enqueue transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	if err := rejectPurged(ctx, queries, request.PubKey); err != nil {
		return err
	}
	if err := upsertEventMapping(ctx, queries, event); err != nil {
		return fmt.Errorf("save bridge event: %w", err)
	}
	if err := enqueueOutbox(ctx, queries, request); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save event and enqueue transaction: %w", err)
	}
	return nil
}

func (s SQLiteStore) EnqueueEvent(ctx context.Context, request EventEnqueueRequest) error {
	if request.Event.PubKey != request.Mapping.AuthorPubKey {
		return ErrAuthorMismatch
	}
	if err := validatePublishRequest(request.Event); err != nil {
		return err
	}
	if err := validateMappingEventID(request.Mapping, request.Event); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin durable event enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	if err := rejectPurged(ctx, queries, request.Event.PubKey); err != nil {
		return err
	}
	count, err := queries.OutboxCount(ctx)
	if err != nil {
		return fmt.Errorf("count outbox items: %w", err)
	}
	registered, err := queries.PublisherRegisteredOrPending(ctx, storesqlc.PublisherRegisteredOrPendingParams{Pubkey: request.Event.PubKey, AggregateKey: request.Event.AggregateKey})
	if err != nil {
		return fmt.Errorf("find publisher registration: %w", err)
	}
	needed := int64(1)
	if !registered {
		needed++
	}
	if request.Limit <= 0 || count+needed > request.Limit {
		return ErrOutboxFull
	}
	if !registered {
		if err := enqueueOutbox(ctx, queries, OutboxRequest{AggregateKey: request.Event.AggregateKey, Operation: OutboxAllowPublisher, PubKey: request.Event.PubKey, AvailableAt: request.Event.AvailableAt}); err != nil {
			return err
		}
	}
	if err := upsertEventMapping(ctx, queries, request.Mapping); err != nil {
		return fmt.Errorf("save bridge event: %w", err)
	}
	if err := enqueueOutbox(ctx, queries, request.Event); err != nil {
		return err
	}
	if request.Cursor != nil {
		if err := saveCursor(ctx, queries, request.Mapping.Source.Scope, request.Cursor); err != nil {
			return fmt.Errorf("save sync cursor: %w", err)
		}
	}
	if request.SourceOperation != "" {
		if err := saveSourceOperation(ctx, queries, request.Mapping.Source, request.SourceOperation); err != nil {
			return fmt.Errorf("save source operation: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit durable event enqueue: %w", err)
	}
	return nil
}

func (s SQLiteStore) Reconcile(ctx context.Context, request ReconciliationRequest) error {
	return s.ReconcileBatch(ctx, ReconciliationBatchRequest{TargetScope: request.Scope, Targets: request.Targets, EventScopes: []SourceScope{request.Scope}, Events: request.Events, Limit: request.Limit})
}

func (s SQLiteStore) ReconcileBatch(ctx context.Context, request ReconciliationBatchRequest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	count, err := queries.OutboxCount(ctx)
	if err != nil {
		return fmt.Errorf("count outbox items: %w", err)
	}
	registered := map[string]bool{}
	pending := make([]EventEnqueueRequest, 0, len(request.Events))
	needed := int64(0)
	allowedScopes := make(map[SourceScope]struct{}, len(request.EventScopes))
	for _, scope := range request.EventScopes {
		allowedScopes[scope] = struct{}{}
	}
	for _, event := range request.Events {
		if _, ok := allowedScopes[event.Mapping.Source.Scope]; !ok {
			return ErrSourceScopeMismatch
		}
		if event.Event.PubKey != event.Mapping.AuthorPubKey {
			return ErrAuthorMismatch
		}
		if err := validatePublishRequest(event.Event); err != nil {
			return err
		}
		if err := validateMappingEventID(event.Mapping, event.Event); err != nil {
			return err
		}
		if err := rejectPurged(ctx, queries, event.Event.PubKey); err != nil {
			return err
		}
		existing, err := queries.EventIDBySourceURI(ctx, eventIDParams(event.Mapping.Source))
		if err == nil {
			if event.SourceOperation != "" {
				operation, opErr := queries.SourceOperationBySourceURI(ctx, operationParams(event.Mapping.Source))
				if opErr == nil && operation == event.SourceOperation {
					continue
				}
				if opErr != nil && opErr != sql.ErrNoRows {
					return fmt.Errorf("find reconciliation identity: %w", opErr)
				}
			} else if existing == event.Mapping.NostrEventID {
				continue
			}
		}
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("find reconciliation mapping: %w", err)
		}
		pending = append(pending, event)
		needed++
		if _, seen := registered[event.Event.PubKey]; !seen {
			allowed, err := queries.PublisherRegisteredOrPending(ctx, storesqlc.PublisherRegisteredOrPendingParams{Pubkey: event.Event.PubKey, AggregateKey: event.Event.AggregateKey})
			if err != nil {
				return fmt.Errorf("find publisher registration: %w", err)
			}
			registered[event.Event.PubKey] = allowed
			if !allowed {
				needed++
			}
		}
	}
	if request.Limit <= 0 || count+needed > request.Limit {
		return ErrOutboxFull
	}
	if err := queries.ReplaceSyncTargetsDelete(ctx, targetScopeParams(request.TargetScope)); err != nil {
		return fmt.Errorf("clear sync targets: %w", err)
	}
	for _, target := range request.Targets {
		if err := queries.InsertSyncTarget(ctx, storesqlc.InsertSyncTargetParams{Provider: request.TargetScope.Provider, SourceAccount: request.TargetScope.Account, Target: target}); err != nil {
			return fmt.Errorf("save sync target: %w", err)
		}
	}
	allowQueued := map[string]bool{}
	for _, event := range pending {
		if !registered[event.Event.PubKey] && !allowQueued[event.Event.PubKey] {
			if err := enqueueOutbox(ctx, queries, OutboxRequest{AggregateKey: event.Event.AggregateKey, Operation: OutboxAllowPublisher, PubKey: event.Event.PubKey, AvailableAt: event.Event.AvailableAt}); err != nil {
				return err
			}
			allowQueued[event.Event.PubKey] = true
		}
		if err := upsertEventMapping(ctx, queries, event.Mapping); err != nil {
			return fmt.Errorf("save reconciliation mapping: %w", err)
		}
		if event.SourceOperation != "" {
			if err := saveSourceOperation(ctx, queries, event.Mapping.Source, event.SourceOperation); err != nil {
				return fmt.Errorf("save reconciliation identity: %w", err)
			}
		}
		if err := enqueueOutbox(ctx, queries, event.Event); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reconciliation: %w", err)
	}
	return nil
}

func (s SQLiteStore) EnqueueDelete(ctx context.Context, request DeleteEnqueueRequest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin durable delete enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	storedAuthor, err := queries.EventAuthorBySourceURI(ctx, authorParams(request.Source))
	if err != nil {
		return fmt.Errorf("find delete mapping author: %w", err)
	}
	if request.Event.PubKey != storedAuthor {
		return ErrAuthorMismatch
	}
	if err := validatePublishRequest(request.Event); err != nil {
		return err
	}
	if err := rejectPurged(ctx, queries, request.Event.PubKey); err != nil {
		return err
	}
	count, err := queries.OutboxCount(ctx)
	if err != nil {
		return fmt.Errorf("count outbox items: %w", err)
	}
	registered, err := queries.PublisherRegisteredOrPending(ctx, storesqlc.PublisherRegisteredOrPendingParams{Pubkey: request.Event.PubKey, AggregateKey: request.Event.AggregateKey})
	if err != nil {
		return err
	}
	needed := int64(1)
	if !registered {
		needed++
	}
	if request.Limit <= 0 || count+needed > request.Limit {
		return ErrOutboxFull
	}
	if !registered {
		if err := enqueueOutbox(ctx, queries, OutboxRequest{AggregateKey: request.Event.AggregateKey, Operation: OutboxAllowPublisher, PubKey: request.Event.PubKey, AvailableAt: request.Event.AvailableAt}); err != nil {
			return err
		}
	}
	if err := queries.DeleteEventMapping(ctx, deleteMappingParams(request.Source)); err != nil {
		return fmt.Errorf("delete bridge event: %w", err)
	}
	if err := queries.DeleteSourceOperation(ctx, deleteOperationParams(request.Source)); err != nil {
		return fmt.Errorf("delete source operation: %w", err)
	}
	if err := enqueueOutbox(ctx, queries, request.Event); err != nil {
		return err
	}
	if request.Cursor != nil {
		if err := saveCursor(ctx, queries, request.Source.Scope, request.Cursor); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit durable delete enqueue: %w", err)
	}
	return nil
}

func (s SQLiteStore) EnqueueUpdate(ctx context.Context, request UpdateEnqueueRequest) error {
	if request.Deletion.PubKey != request.Replacement.PubKey || request.Replacement.PubKey != request.Mapping.AuthorPubKey {
		return ErrAuthorMismatch
	}
	if err := validatePublishRequest(request.Deletion); err != nil {
		return err
	}
	if err := validatePublishRequest(request.Replacement); err != nil {
		return err
	}
	if err := validateMappingEventID(request.Mapping, request.Replacement); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin durable update enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	if err := rejectPurged(ctx, queries, request.Replacement.PubKey); err != nil {
		return err
	}
	count, err := queries.OutboxCount(ctx)
	if err != nil {
		return fmt.Errorf("count outbox items: %w", err)
	}
	allowed, err := queries.PublisherRegisteredOrPending(ctx, storesqlc.PublisherRegisteredOrPendingParams{Pubkey: request.Replacement.PubKey, AggregateKey: request.Replacement.AggregateKey})
	if err != nil {
		return err
	}
	needed := int64(2)
	if !allowed {
		needed++
	}
	if request.Limit <= 0 || count+needed > request.Limit {
		return ErrOutboxFull
	}
	if !allowed {
		if err := enqueueOutbox(ctx, queries, OutboxRequest{AggregateKey: request.Replacement.AggregateKey, Operation: OutboxAllowPublisher, PubKey: request.Replacement.PubKey, AvailableAt: request.Replacement.AvailableAt}); err != nil {
			return err
		}
	}
	if err := enqueueOutbox(ctx, queries, request.Deletion); err != nil {
		return err
	}
	if err := upsertEventMapping(ctx, queries, request.Mapping); err != nil {
		return fmt.Errorf("replace bridge event: %w", err)
	}
	if err := enqueueOutbox(ctx, queries, request.Replacement); err != nil {
		return err
	}
	if request.SourceOperation != "" {
		if err := saveSourceOperation(ctx, queries, request.Mapping.Source, request.SourceOperation); err != nil {
			return fmt.Errorf("save source operation: %w", err)
		}
	}
	if request.Cursor != nil {
		if err := saveCursor(ctx, queries, request.Mapping.Source.Scope, request.Cursor); err != nil {
			return fmt.Errorf("save sync cursor: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit durable update enqueue: %w", err)
	}
	return nil
}

func (s SQLiteStore) EnqueuePurge(ctx context.Context, request PurgeRequest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin purge enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	var deletion nostr.Event
	if err := json.Unmarshal([]byte(request.Payload), &deletion); err != nil || deletion.Kind != 5 || deletion.PubKey.Hex() != request.PubKey || !deletion.CheckID() || !deletion.VerifySignature() {
		return fmt.Errorf("invalid signed purge deletion event")
	}
	tagged := make(map[string]struct{})
	for _, tag := range deletion.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			tagged[tag[1]] = struct{}{}
		}
	}
	ids, err := queries.EventIDsByAuthor(ctx, request.PubKey)
	if err != nil {
		return err
	}
	known := make(map[string]struct{})
	for _, id := range ids {
		known[id] = struct{}{}
	}
	if len(tagged) != len(known) {
		return fmt.Errorf("purge deletion does not cover known events")
	}
	for id := range known {
		if _, ok := tagged[id]; !ok {
			return fmt.Errorf("purge deletion does not cover known events")
		}
	}
	markedEvent, markerErr := queries.PublisherPurgeEvent(ctx, request.PubKey)
	if markerErr == nil {
		if markedEvent == deletion.ID.Hex() {
			return nil
		}
		return ErrPurgePending
	}
	if markerErr != sql.ErrNoRows {
		return markerErr
	}
	unallowExists, err := queries.PublisherUnallowPending(ctx, request.PubKey)
	if err != nil {
		return err
	}
	if unallowExists {
		return ErrPurgeConflict
	}
	registered, err := queries.PublisherRegistered(ctx, request.PubKey)
	if err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("publisher is not registered")
	}
	count, err := queries.OutboxCount(ctx)
	if err != nil {
		return err
	}
	if request.Limit <= 0 || count+2 > request.Limit {
		return ErrOutboxFull
	}
	if err := queries.InsertPublisherPurge(ctx, storesqlc.InsertPublisherPurgeParams{Pubkey: request.PubKey, DeletionEventID: deletion.ID.Hex(), CreatedAt: request.AvailableAt.Unix()}); err != nil {
		return fmt.Errorf("mark publisher purge: %w", err)
	}
	if err := enqueueOutbox(ctx, queries, OutboxRequest{AggregateKey: request.PubKey, Operation: OutboxPublishEvent, PubKey: request.PubKey, Payload: request.Payload, AvailableAt: request.AvailableAt}); err != nil {
		return err
	}
	if err := enqueueOutbox(ctx, queries, OutboxRequest{AggregateKey: request.PubKey, Operation: OutboxUnallowPublisher, PubKey: request.PubKey, AvailableAt: request.AvailableAt}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit purge enqueue: %w", err)
	}
	return nil
}

func (s SQLiteStore) EventIDsByAuthor(ctx context.Context, pubkey string) ([]string, error) {
	ids, err := s.queries.EventIDsByAuthor(ctx, pubkey)
	if err != nil {
		return nil, fmt.Errorf("query author event IDs: %w", err)
	}
	return ids, nil
}

func (s SQLiteStore) ClaimOutbox(ctx context.Context, now time.Time, leaseDuration time.Duration, limit int) ([]OutboxItem, error) {
	if leaseDuration <= 0 {
		return nil, ErrInvalidLease
	}
	if limit <= 0 {
		return []OutboxItem{}, nil
	}
	token, err := newClaimToken()
	if err != nil {
		return nil, err
	}
	claimedUntil := now.Add(leaseDuration)
	rows, err := s.queries.ClaimOutbox(ctx, storesqlc.ClaimOutboxParams{ClaimToken: token, ClaimedUntil: claimedUntil.UnixNano(), NowNanos: now.UnixNano(), NowSeconds: now.Unix(), ResultLimit: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("select claimable outbox items: %w", err)
	}
	var items []OutboxItem
	for _, row := range rows {
		item := outboxItem(row)
		item.ClaimToken = token
		item.ClaimedUntil = claimedUntil
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].AvailableAt.Equal(items[j].AvailableAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].AvailableAt.Before(items[j].AvailableAt)
	})
	return items, nil
}

func newClaimToken() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate outbox claim token: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func (s SQLiteStore) CompleteOutbox(ctx context.Context, id int64, token string, now time.Time) error {
	result, err := s.queries.CompleteOutbox(ctx, storesqlc.CompleteOutboxParams{ID: id, ClaimToken: token, ClaimedUntil: now.UnixNano()})
	if err != nil {
		return fmt.Errorf("complete outbox item: %w", err)
	}
	return requireClaimOwner(result)
}

func (s SQLiteStore) CompletePublisherRegistration(ctx context.Context, id int64, token, pubkey string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin publisher registration completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	result, err := queries.CompleteOutbox(ctx, storesqlc.CompleteOutboxParams{ID: id, ClaimToken: token, ClaimedUntil: now.UnixNano()})
	if err != nil {
		return fmt.Errorf("complete publisher registration outbox item: %w", err)
	}
	if err := requireClaimOwner(result); err != nil {
		return err
	}
	if err := queries.SetPublisherRegistered(ctx, storesqlc.SetPublisherRegisteredParams{Pubkey: pubkey, RegisteredAt: now.Unix()}); err != nil {
		return fmt.Errorf("set publisher registered: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit publisher registration completion: %w", err)
	}
	return nil
}

// RecoverPublisherRegistration atomically replaces a claimed rejected publish
// with allow-publisher followed by the exact same publish in its aggregate.
func (s SQLiteStore) RecoverPublisherRegistration(ctx context.Context, id int64, token, pubkey string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin publisher recovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	claimed, err := queries.ClaimedPublishEvent(ctx, storesqlc.ClaimedPublishEventParams{ID: id, ClaimToken: token, ClaimedUntil: now.UnixNano()})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrClaimLost
	}
	if err != nil {
		return fmt.Errorf("load rejected publish: %w", err)
	}
	if claimed.AggregateKey != pubkey {
		return ErrAuthorMismatch
	}
	if err := queries.ClearPublisherRegistration(ctx, pubkey); err != nil {
		return err
	}
	if err := queries.ResetRejectedPublishAsRegistration(ctx, storesqlc.ResetRejectedPublishAsRegistrationParams{AvailableAt: now.Unix(), ID: id}); err != nil {
		return err
	}
	// Move later items out of the UNIQUE key space before shifting them. The
	// rejected publish must remain ahead of every later aggregate operation.
	if err := queries.NegateLaterOutboxSequences(ctx, storesqlc.NegateLaterOutboxSequencesParams{AggregateKey: claimed.AggregateKey, Sequence: claimed.Sequence}); err != nil {
		return err
	}
	if err := queries.ShiftNegatedOutboxSequences(ctx, claimed.AggregateKey); err != nil {
		return err
	}
	if err := queries.IncrementOutboxLastSequence(ctx, claimed.AggregateKey); err != nil {
		return err
	}
	if err := queries.InsertRecoveredPublish(ctx, storesqlc.InsertRecoveredPublishParams{AggregateKey: claimed.AggregateKey, Sequence: claimed.Sequence + 1, Pubkey: pubkey, Payload: claimed.Payload, AvailableAt: now.Unix()}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit publisher recovery: %w", err)
	}
	return nil
}

func (s SQLiteStore) CompletePublisherUnregistration(ctx context.Context, id int64, token string, now time.Time, pubkey string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin publisher unregistration completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	result, err := queries.CompleteOutbox(ctx, storesqlc.CompleteOutboxParams{ID: id, ClaimToken: token, ClaimedUntil: now.UnixNano()})
	if err != nil {
		return fmt.Errorf("complete publisher unregistration outbox item: %w", err)
	}
	if err := requireClaimOwner(result); err != nil {
		return err
	}
	if err := queries.ClearPublisherRegistration(ctx, pubkey); err != nil {
		return fmt.Errorf("clear publisher registration: %w", err)
	}
	if err := queries.DeletePublisherSourceOperations(ctx, pubkey); err != nil {
		return fmt.Errorf("delete publisher source operations: %w", err)
	}
	if err := queries.DeletePublisherMappings(ctx, pubkey); err != nil {
		return fmt.Errorf("delete publisher mappings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit publisher unregistration completion: %w", err)
	}
	return nil
}

func (s SQLiteStore) RetryOutbox(ctx context.Context, id int64, token string, now, availableAt time.Time, lastError string) error {
	result, err := s.queries.RetryOutbox(ctx, storesqlc.RetryOutboxParams{AvailableAt: availableAt.Unix(), LastError: lastError, ID: id, ClaimToken: token, ClaimedUntil: now.UnixNano()})
	if err != nil {
		return fmt.Errorf("retry outbox item: %w", err)
	}
	return requireClaimOwner(result)
}

func requireClaimOwner(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected outbox rows: %w", err)
	}
	if rows == 0 {
		return ErrClaimLost
	}
	return nil
}

func (s SQLiteStore) OutboxCount(ctx context.Context) (int64, error) {
	count, err := s.queries.OutboxCount(ctx)
	if err != nil {
		return 0, fmt.Errorf("count outbox items: %w", err)
	}
	return count, nil
}

func (s SQLiteStore) SetPublisherRegistered(ctx context.Context, pubkey string, registeredAt time.Time) error {
	err := s.queries.SetPublisherRegistered(ctx, storesqlc.SetPublisherRegisteredParams{Pubkey: pubkey, RegisteredAt: registeredAt.Unix()})
	if err != nil {
		return fmt.Errorf("set publisher registered: %w", err)
	}
	return nil
}

func (s SQLiteStore) ClearPublisherRegistration(ctx context.Context, pubkey string) error {
	if err := s.queries.ClearPublisherRegistration(ctx, pubkey); err != nil {
		return fmt.Errorf("clear publisher registration: %w", err)
	}
	return nil
}

func (s SQLiteStore) PublisherRegistered(ctx context.Context, pubkey string) (bool, error) {
	exists, err := s.queries.PublisherRegistered(ctx, pubkey)
	if err != nil {
		return false, fmt.Errorf("find publisher registration: %w", err)
	}
	return exists, nil
}

func (s SQLiteStore) ReplaceSyncTargets(ctx context.Context, scope SourceScope, targets []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin replace sync targets transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	if err := queries.ReplaceSyncTargetsDelete(ctx, targetScopeParams(scope)); err != nil {
		return fmt.Errorf("clear sync targets: %w", err)
	}
	for _, target := range targets {
		if err := queries.InsertSyncTarget(ctx, storesqlc.InsertSyncTargetParams{Provider: scope.Provider, SourceAccount: scope.Account, Target: target}); err != nil {
			return fmt.Errorf("save sync target: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replace sync targets transaction: %w", err)
	}
	return nil
}

func (s SQLiteStore) SyncTargets(ctx context.Context, scope SourceScope) ([]string, error) {
	dids, err := s.queries.SyncTargets(ctx, storesqlc.SyncTargetsParams{Provider: scope.Provider, SourceAccount: scope.Account})
	if err != nil {
		return nil, fmt.Errorf("query sync targets: %w", err)
	}
	return dids, nil
}

func (s SQLiteStore) EventMappingBySourceURI(ctx context.Context, ref SourceRef) (EventMapping, error) {
	row, err := s.queries.EventMappingBySourceURI(ctx, mappingParams(ref))
	if err != nil {
		return EventMapping{}, fmt.Errorf("find bridge event by source URI: %w", err)
	}
	return eventMapping(row), nil
}

// SaveEventMapping stores source-to-Nostr metadata without retaining relay event JSON.
func (s SQLiteStore) SaveEventMapping(ctx context.Context, mapping EventMapping) error {
	err := upsertEventMapping(ctx, s.queries, mapping)
	if err != nil {
		return fmt.Errorf("save event mapping: %w", err)
	}
	return nil
}

func (s SQLiteStore) DeleteEventBySourceURI(ctx context.Context, ref SourceRef) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete bridge event transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	if err := queries.DeleteEventMapping(ctx, deleteMappingParams(ref)); err != nil {
		return fmt.Errorf("delete bridge event by source URI: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete bridge event transaction: %w", err)
	}
	return nil
}

func (s SQLiteStore) SaveSourceOperation(ctx context.Context, ref SourceRef, identity string) error {
	err := saveSourceOperation(ctx, s.queries, ref, identity)
	if err != nil {
		return fmt.Errorf("save source operation: %w", err)
	}
	return nil
}

func (s SQLiteStore) SourceOperationBySourceURI(ctx context.Context, ref SourceRef) (string, error) {
	identity, err := s.queries.SourceOperationBySourceURI(ctx, operationParams(ref))
	if err != nil {
		return "", fmt.Errorf("find source operation by source URI: %w", err)
	}
	return identity, nil
}

func (s SQLiteStore) SaveCursor(ctx context.Context, scope SourceScope, name, value string) error {
	err := saveCursor(ctx, s.queries, scope, &CursorUpdate{Name: name, Value: value})
	if err != nil {
		return fmt.Errorf("save sync cursor: %w", err)
	}
	return nil
}

func (s SQLiteStore) Cursor(ctx context.Context, scope SourceScope, name string) (string, error) {
	value, err := s.queries.Cursor(ctx, storesqlc.CursorParams{Provider: scope.Provider, SourceAccount: scope.Account, Name: name})
	if err != nil {
		return "", fmt.Errorf("find sync cursor: %w", err)
	}
	return value, nil
}

func (s SQLiteStore) SaveOAuthSession(ctx context.Context, scope SourceScope, session OAuthSession) error {
	err := s.queries.SaveOAuthSession(ctx, storesqlc.SaveOAuthSessionParams{Provider: scope.Provider, SourceAccount: scope.Account, State: session.State, EncryptedPayload: session.EncryptedPayload, ExpiresAt: session.ExpiresAt})
	if err != nil {
		return fmt.Errorf("save OAuth session: %w", err)
	}
	return nil
}

func (s SQLiteStore) OAuthSessionByState(ctx context.Context, scope SourceScope, state string) (OAuthSession, error) {
	row, err := s.queries.OAuthSessionByState(ctx, storesqlc.OAuthSessionByStateParams{Provider: scope.Provider, SourceAccount: scope.Account, State: state})
	if err != nil {
		return OAuthSession{}, fmt.Errorf("find OAuth session by state: %w", err)
	}
	return oauthSession(row), nil
}

func (s SQLiteStore) DeleteOAuthSession(ctx context.Context, scope SourceScope, state string) error {
	if err := s.queries.DeleteOAuthSession(ctx, storesqlc.DeleteOAuthSessionParams{Provider: scope.Provider, SourceAccount: scope.Account, State: state}); err != nil {
		return fmt.Errorf("delete OAuth session: %w", err)
	}
	return nil
}

func (s SQLiteStore) SaveOAuthToken(ctx context.Context, scope SourceScope, token OAuthToken) error {
	err := s.queries.SaveOAuthToken(ctx, storesqlc.SaveOAuthTokenParams{Provider: scope.Provider, SourceAccount: scope.Account, AccountDid: token.AccountDID, EncryptedPayload: token.EncryptedPayload, UpdatedAt: token.UpdatedAt})
	if err != nil {
		return fmt.Errorf("save OAuth token: %w", err)
	}
	return nil
}

func (s SQLiteStore) OAuthTokenByAccountDID(ctx context.Context, scope SourceScope, accountDID string) (OAuthToken, error) {
	row, err := s.queries.OAuthTokenByAccountDID(ctx, storesqlc.OAuthTokenByAccountDIDParams{Provider: scope.Provider, SourceAccount: scope.Account, AccountDid: accountDID})
	if err != nil {
		return OAuthToken{}, fmt.Errorf("find OAuth token by account DID: %w", err)
	}
	return oauthToken(row), nil
}
