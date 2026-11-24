package store

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"

	"fiatjaf.com/nostr"
)

func (s *SQLiteStore) QueryEvents(ctx context.Context, filter nostr.Filter) (iter.Seq[nostr.Event], error) {
	query, args, empty := buildEventQuery(filter)
	if empty {
		return func(func(nostr.Event) bool) {}, nil
	}

	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []nostr.Event
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var event nostr.Event
		if err := json.Unmarshal([]byte(encoded), &event); err != nil {
			return nil, fmt.Errorf("decode stored event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return func(yield func(nostr.Event) bool) {
		for _, event := range events {
			if !yield(event) {
				return
			}
		}
	}, nil
}

// buildEventQuery builds a parameterized SQL query for the given filter.
// It returns empty == true when the filter cannot match any event.
func buildEventQuery(filter nostr.Filter) (string, []any, bool) {
	if filter.LimitZero ||
		(filter.IDs != nil && len(filter.IDs) == 0) ||
		(filter.Authors != nil && len(filter.Authors) == 0) ||
		(filter.Kinds != nil && len(filter.Kinds) == 0) ||
		(filter.Since != 0 && filter.Until != 0 && filter.Since > filter.Until) {
		return "", nil, true
	}

	tagNames := slices.Sorted(maps.Keys(filter.Tags))
	for _, tagName := range tagNames {
		if len(filter.Tags[tagName]) == 0 {
			return "", nil, true
		}
	}

	var query strings.Builder
	var args []any

	query.WriteString("SELECT events.event_json FROM ")
	remainingTags := tagNames
	if len(filter.IDs) == 0 && len(tagNames) > 0 {
		firstTag := tagNames[0]
		firstValues := filter.Tags[firstTag]
		query.WriteString("(SELECT DISTINCT event_id FROM event_tags WHERE tag_name = ? AND tag_value IN (")
		writePlaceholders(&query, len(firstValues))
		query.WriteString(")) AS matched_tags CROSS JOIN events ON events.id = matched_tags.event_id")
		args = appendTagFilterArgs(args, firstTag, firstValues)
		remainingTags = tagNames[1:]
	} else {
		query.WriteString("events")
	}

	hasWhere := false
	if len(filter.IDs) > 0 {
		appendInClause(&query, &hasWhere, "events.id", len(filter.IDs))
		for _, id := range filter.IDs {
			args = append(args, id.Hex())
		}
	}
	if len(filter.Authors) > 0 {
		appendInClause(&query, &hasWhere, "events.pubkey", len(filter.Authors))
		for _, author := range filter.Authors {
			args = append(args, author.Hex())
		}
	}
	if len(filter.Kinds) > 0 {
		appendInClause(&query, &hasWhere, "events.kind", len(filter.Kinds))
		for _, kind := range filter.Kinds {
			args = append(args, int64(kind))
		}
	}
	if filter.Since != 0 {
		writeWhereOrAnd(&query, &hasWhere)
		query.WriteString("events.created_at >= ?")
		args = append(args, int64(filter.Since))
	}
	if filter.Until != 0 {
		writeWhereOrAnd(&query, &hasWhere)
		query.WriteString("events.created_at <= ?")
		args = append(args, int64(filter.Until))
	}
	for _, tagName := range remainingTags {
		values := filter.Tags[tagName]
		writeWhereOrAnd(&query, &hasWhere)
		query.WriteString("events.id IN (SELECT event_id FROM event_tags WHERE tag_name = ? AND tag_value IN (")
		writePlaceholders(&query, len(values))
		query.WriteString("))")
		args = appendTagFilterArgs(args, tagName, values)
	}

	query.WriteString(" ORDER BY events.created_at DESC, events.id ASC")
	if filter.Limit > 0 {
		query.WriteString(" LIMIT ?")
		args = append(args, filter.Limit)
	}

	return query.String(), args, false
}

func appendInClause(b *strings.Builder, hasWhere *bool, column string, count int) {
	writeWhereOrAnd(b, hasWhere)
	b.WriteString(column)
	b.WriteString(" IN (")
	writePlaceholders(b, count)
	b.WriteByte(')')
}

func writeWhereOrAnd(b *strings.Builder, hasWhere *bool) {
	if *hasWhere {
		b.WriteString(" AND ")
		return
	}
	b.WriteString(" WHERE ")
	*hasWhere = true
}

func writePlaceholders(b *strings.Builder, count int) {
	for i := range count {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('?')
	}
}

func appendTagFilterArgs(args []any, tagName string, values []string) []any {
	args = append(args, tagName)
	for _, v := range values {
		args = append(args, v)
	}
	return args
}
