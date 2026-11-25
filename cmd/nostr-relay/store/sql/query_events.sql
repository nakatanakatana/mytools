-- name: QueryEvents :many
-- @param ids_json: text
-- @param authors_json: text
-- @param kinds_json: text
-- @param since: integer
-- @param until: integer
-- @param extra_tag_name: text
-- @param extra_tag_values_json: text
-- @param result_limit: integer
SELECT events.event_json
FROM events
WHERE TRUE
@if-present(ids_json)
  AND events.id IN (SELECT value FROM json_each(:ids_json))
@endif
@if-present(authors_json)
  AND events.pubkey IN (SELECT value FROM json_each(:authors_json))
@endif
@if-present(kinds_json)
  AND events.kind IN (SELECT value FROM json_each(:kinds_json))
@endif
@if-present(since)
  AND events.created_at >= :since
@endif
@if-present(until)
  AND events.created_at <= :until
@endif
  AND @filter-tree(extra_tags)
@predicate(tag_match)
  events.id IN (
      SELECT event_id
      FROM event_tags
      WHERE tag_name = :extra_tag_name
        AND tag_value IN (SELECT value FROM json_each(:extra_tag_values_json))
  )
@end
ORDER BY events.created_at DESC, events.id ASC
LIMIT :result_limit;

-- name: QueryEventsByTag :many
-- @param tag_name: text
-- @param tag_values_json: text
-- @param authors_json: text
-- @param kinds_json: text
-- @param since: integer
-- @param until: integer
-- @param extra_tag_name: text
-- @param extra_tag_values_json: text
-- @param result_limit: integer
SELECT events.event_json
FROM (
    SELECT DISTINCT event_id
    FROM event_tags
    WHERE tag_name = :tag_name
      AND tag_value IN (SELECT value FROM json_each(:tag_values_json))
) AS matched_tags
CROSS JOIN events ON events.id = matched_tags.event_id
WHERE TRUE
@if-present(authors_json)
  AND events.pubkey IN (SELECT value FROM json_each(:authors_json))
@endif
@if-present(kinds_json)
  AND events.kind IN (SELECT value FROM json_each(:kinds_json))
@endif
@if-present(since)
  AND events.created_at >= :since
@endif
@if-present(until)
  AND events.created_at <= :until
@endif
  AND @filter-tree(extra_tags)
@predicate(tag_match)
  events.id IN (
      SELECT event_id
      FROM event_tags
      WHERE tag_name = :extra_tag_name
        AND tag_value IN (SELECT value FROM json_each(:extra_tag_values_json))
  )
@end
ORDER BY events.created_at DESC, events.id ASC
LIMIT :result_limit;
