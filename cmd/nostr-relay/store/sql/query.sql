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

-- name: AllEvents :many
SELECT event_json
FROM events
WHERE
    (NOT sqlc.arg(has_ids) OR id IN (SELECT value FROM json_each(sqlc.arg(ids_json))))
    AND (NOT sqlc.arg(has_authors) OR pubkey IN (SELECT value FROM json_each(sqlc.arg(authors_json))))
    AND (NOT sqlc.arg(has_kinds) OR kind IN (SELECT value FROM json_each(sqlc.arg(kinds_json))))
    AND (NOT sqlc.arg(has_since) OR created_at >= sqlc.arg(since))
    AND (NOT sqlc.arg(has_until) OR created_at <= sqlc.arg(until))
    AND (NOT sqlc.arg(has_tags) OR NOT EXISTS (
        SELECT 1
        FROM json_each(sqlc.arg(tags_json)) AS requested_tag
        WHERE NOT EXISTS (
            SELECT 1
            FROM json_each(json_extract(events.event_json, '$.tags')) AS event_tag
            WHERE json_extract(event_tag.value, '$[0]') = requested_tag.key
              AND json_extract(event_tag.value, '$[1]') IN (
                  SELECT value FROM json_each(requested_tag.value)
              )
        )
    ))
ORDER BY created_at DESC, id ASC
LIMIT CASE WHEN sqlc.arg(result_limit) > 0 THEN sqlc.arg(result_limit) ELSE -1 END;

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
