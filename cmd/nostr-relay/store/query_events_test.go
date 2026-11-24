package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

func TestBuildEventQuery(t *testing.T) {
	t.Parallel()

	id1 := nostr.ID{1}
	id2 := nostr.ID{2}
	pk1 := nostr.PubKey{1}

	tests := []struct {
		name      string
		filter    nostr.Filter
		wantSQL   string
		wantArgs  []any
		wantEmpty bool
	}{
		{
			name:      "limit zero short circuits",
			filter:    nostr.Filter{LimitZero: true},
			wantEmpty: true,
		},
		{
			name:      "empty ids slice short circuits",
			filter:    nostr.Filter{IDs: []nostr.ID{}},
			wantEmpty: true,
		},
		{
			name:      "empty authors slice short circuits",
			filter:    nostr.Filter{Authors: []nostr.PubKey{}},
			wantEmpty: true,
		},
		{
			name:      "empty kinds slice short circuits",
			filter:    nostr.Filter{Kinds: []nostr.Kind{}},
			wantEmpty: true,
		},
		{
			name:      "since greater than until short circuits",
			filter:    nostr.Filter{Since: 20, Until: 10},
			wantEmpty: true,
		},
		{
			name:      "empty tag values slice short circuits",
			filter:    nostr.Filter{Tags: nostr.TagMap{"e": {}}},
			wantEmpty: true,
		},
		{
			name:     "ids only",
			filter:   nostr.Filter{IDs: []nostr.ID{id1, id2}},
			wantSQL:  "SELECT events.event_json FROM events WHERE events.id IN (?, ?) ORDER BY events.created_at DESC, events.id ASC",
			wantArgs: []any{id1.Hex(), id2.Hex()},
		},
		{
			name: "authors kinds since until limit",
			filter: nostr.Filter{
				Authors: []nostr.PubKey{pk1},
				Kinds:   []nostr.Kind{1, 6},
				Since:   100,
				Until:   200,
				Limit:   50,
			},
			wantSQL:  "SELECT events.event_json FROM events WHERE events.pubkey IN (?) AND events.kind IN (?, ?) AND events.created_at >= ? AND events.created_at <= ? ORDER BY events.created_at DESC, events.id ASC LIMIT ?",
			wantArgs: []any{pk1.Hex(), int64(1), int64(6), int64(100), int64(200), 50},
		},
		{
			name: "tag driven query with authors and kinds",
			filter: nostr.Filter{
				Authors: []nostr.PubKey{pk1},
				Kinds:   []nostr.Kind{6, 7},
				Tags:    nostr.TagMap{"e": {id1.Hex()}},
			},
			wantSQL:  "SELECT events.event_json FROM (SELECT DISTINCT event_id FROM event_tags WHERE tag_name = ? AND tag_value IN (?)) AS matched_tags CROSS JOIN events ON events.id = matched_tags.event_id WHERE events.pubkey IN (?) AND events.kind IN (?, ?) ORDER BY events.created_at DESC, events.id ASC",
			wantArgs: []any{"e", id1.Hex(), pk1.Hex(), int64(6), int64(7)},
		},
		{
			name: "multiple tags sorted deterministically",
			filter: nostr.Filter{
				Tags: nostr.TagMap{
					"p": {pk1.Hex()},
					"e": {id1.Hex(), id2.Hex()},
				},
			},
			wantSQL:  "SELECT events.event_json FROM (SELECT DISTINCT event_id FROM event_tags WHERE tag_name = ? AND tag_value IN (?, ?)) AS matched_tags CROSS JOIN events ON events.id = matched_tags.event_id WHERE events.id IN (SELECT event_id FROM event_tags WHERE tag_name = ? AND tag_value IN (?)) ORDER BY events.created_at DESC, events.id ASC",
			wantArgs: []any{"e", id1.Hex(), id2.Hex(), "p", pk1.Hex()},
		},
		{
			name: "ids plus tag drives from events primary key",
			filter: nostr.Filter{
				IDs:  []nostr.ID{id1},
				Tags: nostr.TagMap{"e": {id2.Hex()}},
			},
			wantSQL:  "SELECT events.event_json FROM events WHERE events.id IN (?) AND events.id IN (SELECT event_id FROM event_tags WHERE tag_name = ? AND tag_value IN (?)) ORDER BY events.created_at DESC, events.id ASC",
			wantArgs: []any{id1.Hex(), "e", id2.Hex()},
		},
		{
			name:    "empty filter",
			filter:  nostr.Filter{},
			wantSQL: "SELECT events.event_json FROM events ORDER BY events.created_at DESC, events.id ASC",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotSQL, gotArgs, gotEmpty := buildEventQuery(tt.filter)
			if gotEmpty != tt.wantEmpty {
				t.Fatalf("buildEventQuery() empty = %v, want %v", gotEmpty, tt.wantEmpty)
			}
			if gotSQL != tt.wantSQL {
				t.Fatalf("buildEventQuery() sql =\n  %q\nwant\n  %q", gotSQL, tt.wantSQL)
			}
			if len(gotArgs) == 0 && len(tt.wantArgs) == 0 {
				return
			}
			if !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Fatalf("buildEventQuery() args = %#v, want %#v", gotArgs, tt.wantArgs)
			}
		})
	}
}

func explainQueryPlan(t *testing.T, db *sql.DB, filter nostr.Filter) string {
	t.Helper()
	query, args, empty := buildEventQuery(filter)
	if empty {
		t.Fatalf("buildEventQuery(%#v) unexpectedly returned empty", filter)
	}
	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN failed: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var lines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan EXPLAIN QUERY PLAN row: %v", err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate EXPLAIN QUERY PLAN rows: %v", err)
	}
	return strings.Join(lines, "\n")
}

func TestQueryEventsExplainQueryPlan(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTestStore(t, filepath.Join(t.TempDir(), "relay.sqlite"))

	targetID := nostr.ID{0xaa}
	for i := range 20 {
		ev := testEvent(byte(i+1), testPubKey2, nostr.Timestamp(100+i), 7, nostr.Tags{
			{"e", targetID.Hex()},
			{"p", nostr.NUMS.Hex()},
		}, "reaction")
		if err := s.SaveEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name           string
		filter         nostr.Filter
		wantContains   []string
		wantNotContain []string
	}{
		{
			name:   "ids only uses events primary key",
			filter: nostr.Filter{IDs: []nostr.ID{targetID}},
			wantContains: []string{
				"SEARCH events USING",
				"sqlite_autoindex_events_1",
			},
			wantNotContain: []string{"SCAN events"},
		},
		{
			name: "e tag with authors and kinds uses event_tags index then events pk",
			filter: nostr.Filter{
				Authors: []nostr.PubKey{testPubKey2},
				Kinds:   []nostr.Kind{6, 7},
				Tags:    nostr.TagMap{"e": {targetID.Hex()}},
			},
			wantContains: []string{
				"event_tags_name_value_event_idx",
				"SEARCH events USING",
				"sqlite_autoindex_events_1",
			},
			wantNotContain: []string{
				"SCAN events",
				"events_pubkey_kind_created_at_idx",
			},
		},
		{
			name: "e tag only uses event_tags index",
			filter: nostr.Filter{
				Tags: nostr.TagMap{"e": {targetID.Hex()}},
			},
			wantContains: []string{
				"event_tags_name_value_event_idx",
			},
			wantNotContain: []string{"SCAN events"},
		},
		{
			name: "authors and kinds uses pubkey kind created_at index",
			filter: nostr.Filter{
				Authors: []nostr.PubKey{testPubKey2},
				Kinds:   []nostr.Kind{7},
			},
			wantContains: []string{
				"events_pubkey_kind_created_at_idx",
			},
			wantNotContain: []string{"SCAN events"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := explainQueryPlan(t, s.readDB, tt.filter)
			t.Logf("plan:\n%s", plan)
			for _, want := range tt.wantContains {
				if !strings.Contains(plan, want) {
					t.Errorf("plan missing %q:\n%s", want, plan)
				}
			}
			for _, forbidden := range tt.wantNotContain {
				if strings.Contains(plan, forbidden) {
					t.Errorf("plan unexpectedly contains %q:\n%s", forbidden, plan)
				}
			}
		})
	}
}

func TestQueryEventsFilterCombinations(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTestStore(t, filepath.Join(t.TempDir(), "relay.sqlite"))

	ref1 := nostr.ID{0x10}
	ref2 := nostr.ID{0x20}

	e1 := testEvent(1, nostr.NUMS, 100, 1, nostr.Tags{{"e", ref1.Hex()}, {"p", testPubKey2.Hex()}}, "e1")
	e2 := testEvent(2, nostr.NUMS, 200, 1, nostr.Tags{{"e", ref2.Hex()}, {"p", nostr.NUMS.Hex()}}, "e2")
	e3 := testEvent(3, testPubKey2, 200, 1, nostr.Tags{{"e", ref1.Hex()}, {"p", nostr.NUMS.Hex()}}, "e3")
	e4 := testEvent(4, testPubKey2, 300, 7, nostr.Tags{{"e", ref1.Hex()}, {"p", testPubKey2.Hex()}}, "e4")

	for _, ev := range []nostr.Event{e1, e2, e3, e4} {
		if err := s.SaveEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	query := func(t *testing.T, filter nostr.Filter) []nostr.Event {
		t.Helper()
		seq, err := s.QueryEvents(ctx, filter)
		if err != nil {
			t.Fatalf("QueryEvents(%#v) error = %v", filter, err)
		}
		return slices.Collect(seq)
	}

	t.Run("multi-tag AND", func(t *testing.T) {
		got := query(t, nostr.Filter{
			Tags: nostr.TagMap{
				"e": {ref1.Hex()},
				"p": {testPubKey2.Hex()},
			},
		})
		want := []nostr.Event{e4, e1}
		if !slices.EqualFunc(got, want, eventsEqual) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})

	t.Run("single-tag multi-value OR", func(t *testing.T) {
		got := query(t, nostr.Filter{
			Tags: nostr.TagMap{
				"e": {ref1.Hex(), ref2.Hex()},
			},
		})
		want := []nostr.Event{e4, e2, e3, e1}
		if !slices.EqualFunc(got, want, eventsEqual) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})

	t.Run("since and until inclusive", func(t *testing.T) {
		got := query(t, nostr.Filter{Since: 100, Until: 200})
		want := []nostr.Event{e2, e3, e1}
		if !slices.EqualFunc(got, want, eventsEqual) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})

	t.Run("limit", func(t *testing.T) {
		got := query(t, nostr.Filter{Limit: 2})
		want := []nostr.Event{e4, e2}
		if !slices.EqualFunc(got, want, eventsEqual) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})

	t.Run("LimitZero", func(t *testing.T) {
		got := query(t, nostr.Filter{LimitZero: true})
		if len(got) != 0 {
			t.Fatalf("got %d events, want 0", len(got))
		}
	})

	t.Run("empty slices short circuit", func(t *testing.T) {
		filters := []nostr.Filter{
			{IDs: []nostr.ID{}},
			{Authors: []nostr.PubKey{}},
			{Kinds: []nostr.Kind{}},
			{Tags: nostr.TagMap{"e": {}}},
			{Since: 300, Until: 100},
		}
		for _, f := range filters {
			if got := query(t, f); len(got) != 0 {
				t.Fatalf("filter %#v returned %d events, want 0", f, len(got))
			}
		}
	})

	t.Run("tie breaking on equal created_at by id ASC", func(t *testing.T) {
		got := query(t, nostr.Filter{Since: 200, Until: 200})
		want := []nostr.Event{e2, e3}
		if !slices.EqualFunc(got, want, eventsEqual) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})
}
