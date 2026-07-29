-- name: AdvanceOutboxSequence :exec
INSERT INTO outbox_sequences(aggregate_key, last_sequence) VALUES(?, 1)
ON CONFLICT(aggregate_key) DO UPDATE SET last_sequence=last_sequence+1;

-- name: EnqueueOutbox :exec
INSERT INTO outbox (aggregate_key, `sequence`, operation, pubkey, payload, available_at)
SELECT sqlc.arg(aggregate_key), last_sequence, sqlc.arg(operation), sqlc.arg(pubkey), sqlc.arg(payload), sqlc.arg(available_at)
FROM outbox_sequences WHERE outbox_sequences.aggregate_key=sqlc.arg(sequence_aggregate_key);

-- name: PublisherPurged :one
SELECT EXISTS(SELECT 1 FROM publisher_purges WHERE publisher_purges.pubkey=sqlc.arg(pubkey));

-- name: UpsertEventMapping :exec
INSERT INTO bridge_events(provider,source_account,source_uri,nostr_event_id,source_kind,author_pubkey,updated_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(provider,source_account,source_uri) DO UPDATE SET nostr_event_id=excluded.nostr_event_id,source_kind=excluded.source_kind,author_pubkey=excluded.author_pubkey,updated_at=excluded.updated_at;

-- name: OutboxCount :one
SELECT COUNT(*) FROM outbox;

-- name: PublisherRegisteredOrPending :one
SELECT EXISTS(SELECT 1 FROM publisher_registrations WHERE publisher_registrations.pubkey=sqlc.arg(pubkey) UNION ALL SELECT 1 FROM outbox WHERE aggregate_key=sqlc.arg(aggregate_key) AND operation='allow-publisher');

-- name: EventIDBySourceURI :one
SELECT nostr_event_id FROM bridge_events WHERE provider=? AND source_account=? AND source_uri=?;

-- name: EventAuthorBySourceURI :one
SELECT author_pubkey FROM bridge_events WHERE provider=? AND source_account=? AND source_uri=?;

-- name: ReplaceSyncTargetsDelete :exec
DELETE FROM sync_targets WHERE provider=? AND source_account=?;

-- name: InsertSyncTarget :exec
INSERT OR IGNORE INTO sync_targets(provider,source_account,target) VALUES(?,?,?);

-- name: DeleteEventMapping :exec
DELETE FROM bridge_events WHERE provider=? AND source_account=? AND source_uri=?;

-- name: DeleteSourceOperation :exec
DELETE FROM source_operations WHERE provider=? AND source_account=? AND source_uri=?;

-- name: EventIDsByAuthor :many
SELECT DISTINCT nostr_event_id FROM bridge_events WHERE author_pubkey=? ORDER BY nostr_event_id;

-- name: PublisherPurgeEvent :one
SELECT deletion_event_id FROM publisher_purges WHERE pubkey=?;

-- name: PublisherUnallowPending :one
SELECT EXISTS(SELECT 1 FROM outbox WHERE aggregate_key=? AND operation='unallow-publisher');

-- name: PublisherRegistered :one
SELECT EXISTS(SELECT 1 FROM publisher_registrations WHERE pubkey=?);

-- name: InsertPublisherPurge :exec
INSERT INTO publisher_purges(pubkey,deletion_event_id,created_at) VALUES(?,?,?);

-- name: ClaimOutbox :many
UPDATE outbox SET claim_token=sqlc.arg(claim_token), claimed_until=sqlc.arg(claimed_until)
WHERE id IN (
 SELECT o.id FROM outbox o WHERE (o.claim_token = '' OR o.claimed_until <= sqlc.arg(now_nanos)) AND o.available_at <= sqlc.arg(now_seconds)
 AND NOT EXISTS (SELECT 1 FROM outbox prior WHERE prior.aggregate_key=o.aggregate_key AND prior.`sequence`<o.`sequence`)
 ORDER BY o.available_at, o.id LIMIT sqlc.arg(result_limit)
) AND (claim_token = '' OR claimed_until <= sqlc.arg(now_nanos))
RETURNING id, aggregate_key, `sequence`, operation, pubkey, payload, attempts, available_at, last_error;

-- name: CompleteOutbox :execresult
DELETE FROM outbox WHERE id=? AND claim_token=? AND claim_token<>'' AND claimed_until>?;

-- name: SetPublisherRegistered :exec
INSERT INTO publisher_registrations(pubkey, registered_at) VALUES(?, ?)
ON CONFLICT(pubkey) DO UPDATE SET registered_at=excluded.registered_at;

-- name: ClaimedPublishEvent :one
SELECT aggregate_key,`sequence`,payload FROM outbox WHERE id=? AND claim_token=? AND claim_token<>'' AND claimed_until>? AND operation='publish-event';

-- name: ClearPublisherRegistration :exec
DELETE FROM publisher_registrations WHERE pubkey=?;

-- name: ResetRejectedPublishAsRegistration :exec
UPDATE outbox SET operation='allow-publisher',payload='',attempts=0,last_error='',claim_token='',claimed_until=0,available_at=? WHERE id=?;

-- name: NegateLaterOutboxSequences :exec
UPDATE outbox SET `sequence`=-`sequence` WHERE aggregate_key=? AND `sequence`>?;

-- name: ShiftNegatedOutboxSequences :exec
UPDATE outbox SET `sequence`=(-`sequence`)+1 WHERE aggregate_key=? AND `sequence`<0;

-- name: IncrementOutboxLastSequence :exec
UPDATE outbox_sequences SET last_sequence=last_sequence+1 WHERE aggregate_key=?;

-- name: InsertRecoveredPublish :exec
INSERT INTO outbox(aggregate_key,`sequence`,operation,pubkey,payload,available_at) VALUES(?,?,'publish-event',?,?,?);

-- name: DeletePublisherSourceOperations :exec
DELETE FROM source_operations
WHERE EXISTS (
    SELECT 1 FROM bridge_events
    WHERE bridge_events.provider=source_operations.provider
      AND bridge_events.source_account=source_operations.source_account
      AND bridge_events.source_uri=source_operations.source_uri
      AND bridge_events.author_pubkey=?
);

-- name: DeletePublisherMappings :exec
DELETE FROM bridge_events WHERE author_pubkey=?;

-- name: RetryOutbox :execresult
UPDATE outbox SET attempts=attempts+1, available_at=?, last_error=?, claim_token='', claimed_until=0 WHERE id=? AND claim_token=? AND claim_token<>'' AND claimed_until>?;

-- name: SyncTargets :many
SELECT target FROM sync_targets WHERE provider=? AND source_account=? ORDER BY target;

-- name: EventMappingBySourceURI :one
SELECT provider, source_account, source_uri, nostr_event_id, source_kind, author_pubkey, updated_at FROM bridge_events WHERE provider=? AND source_account=? AND source_uri=?;

-- name: SaveSourceOperation :exec
INSERT INTO source_operations(provider,source_account,source_uri,`identity`) VALUES(?,?,?,?) ON CONFLICT(provider,source_account,source_uri) DO UPDATE SET `identity`=excluded.`identity`;

-- name: SourceOperationBySourceURI :one
SELECT `identity` AS operation_identity FROM source_operations WHERE provider=? AND source_account=? AND source_uri=?;

-- name: SaveCursor :exec
INSERT INTO sync_cursors(provider,source_account,name,`value`) VALUES(?,?,?,?) ON CONFLICT(provider,source_account,name) DO UPDATE SET `value`=excluded.`value`;

-- name: Cursor :one
SELECT `value` AS cursor_value FROM sync_cursors WHERE provider=? AND source_account=? AND name=?;

-- name: SaveOAuthSession :exec
INSERT INTO oauth_sessions(provider,source_account,state,encrypted_payload,expires_at) VALUES(?,?,?,?,?) ON CONFLICT(provider,source_account,state) DO UPDATE SET encrypted_payload=excluded.encrypted_payload,expires_at=excluded.expires_at;

-- name: OAuthSessionByState :one
SELECT provider,source_account,state,encrypted_payload,expires_at FROM oauth_sessions WHERE provider=? AND source_account=? AND state=?;

-- name: DeleteOAuthSession :exec
DELETE FROM oauth_sessions WHERE provider=? AND source_account=? AND state=?;

-- name: SaveOAuthToken :exec
INSERT INTO oauth_tokens(provider,source_account,account_did,encrypted_payload,updated_at,last_refresh_at,reauth_required,last_refresh_error_class) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(provider,source_account,account_did) DO UPDATE SET encrypted_payload=excluded.encrypted_payload,updated_at=excluded.updated_at,last_refresh_at=excluded.last_refresh_at,reauth_required=excluded.reauth_required,last_refresh_error_class=excluded.last_refresh_error_class;

-- name: OAuthTokenByAccountDID :one
SELECT provider,source_account,account_did,encrypted_payload,updated_at,last_refresh_at,reauth_required,last_refresh_error_class FROM oauth_tokens WHERE provider=? AND source_account=? AND account_did=?;

-- name: UpdateOAuthTokenRefreshFailure :exec
UPDATE oauth_tokens
SET last_refresh_error_class=?, reauth_required=?
WHERE provider=? AND source_account=? AND account_did=?;
