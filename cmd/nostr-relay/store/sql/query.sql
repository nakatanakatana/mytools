-- name: EventTombstoned :one
SELECT EXISTS(
    SELECT 1
    FROM deletion_tombstones
    WHERE owner_pubkey = ? AND target_event_id = ?
);

-- name: EventsByPubKeyAndKind :many
SELECT id, event_json
FROM events
WHERE pubkey = ? AND kind = ?;

-- name: DeleteEventByID :exec
DELETE FROM events WHERE id = ?;

-- name: InsertEvent :exec
INSERT OR IGNORE INTO events (id, pubkey, created_at, kind, event_json)
VALUES (?, ?, ?, ?, ?);

-- name: InsertDeletionTombstone :exec
INSERT OR IGNORE INTO deletion_tombstones
    (owner_pubkey, target_event_id, deletion_event_id, created_at)
VALUES (?, ?, ?, ?);

-- name: DeleteOwnedEvent :exec
DELETE FROM events WHERE id = ? AND pubkey = ?;

-- name: EventByID :one
SELECT event_json FROM events WHERE id = ?;

-- name: AllowPublisher :exec
INSERT INTO publisher_allowlist (pubkey, reason, created_at)
VALUES (?, ?, ?)
ON CONFLICT(pubkey) DO UPDATE
SET reason = excluded.reason, created_at = excluded.created_at;

-- name: UnallowPublisher :exec
DELETE FROM publisher_allowlist WHERE pubkey = ?;

-- name: PublisherAllowed :one
SELECT EXISTS(SELECT 1 FROM publisher_allowlist WHERE pubkey = ?);

-- name: ListPublishers :many
SELECT pubkey, reason, created_at
FROM publisher_allowlist
ORDER BY pubkey;

-- name: DeleteExpiredNIP98Events :exec
DELETE FROM nip98_seen_events WHERE seen_at < ?;

-- name: ConsumeNIP98Event :execresult
INSERT OR IGNORE INTO nip98_seen_events(event_id, seen_at) VALUES(?, ?);

-- name: InsertEventTag :exec
INSERT OR IGNORE INTO event_tags (event_id, tag_name, tag_value)
VALUES (?, ?, ?);

-- name: MetaValue :one
SELECT value FROM relay_meta WHERE key = ?;

-- name: SetMetaValue :exec
INSERT INTO relay_meta (key, value)
VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value;

-- name: ListEventsForTagBackfill :many
SELECT id, event_json FROM events;
