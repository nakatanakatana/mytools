package store

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

func TestPrepareEventQuery(t *testing.T) {
	t.Parallel()

	id1 := nostr.ID{1}
	id2 := nostr.ID{2}
	pk1 := nostr.PubKey{1}

	t.Run("short circuits impossible filters", func(t *testing.T) {
		t.Parallel()
		filters := []nostr.Filter{
			{LimitZero: true},
			{IDs: []nostr.ID{}},
			{Authors: []nostr.PubKey{}},
			{Kinds: []nostr.Kind{}},
			{Since: 20, Until: 10},
			{Tags: nostr.TagMap{"e": {}}},
		}
		for _, filter := range filters {
			got, err := prepareEventQuery(filter)
			if err != nil {
				t.Fatalf("prepareEventQuery(%#v) error = %v", filter, err)
			}
			if !got.empty {
				t.Errorf("prepareEventQuery(%#v) empty = false, want true", filter)
			}
		}
	})

	t.Run("ids only dispatches to QueryEvents", func(t *testing.T) {
		t.Parallel()
		got, err := prepareEventQuery(nostr.Filter{IDs: []nostr.ID{id1, id2}})
		if err != nil {
			t.Fatal(err)
		}
		if got.empty || got.byTag {
			t.Fatalf("got empty=%v byTag=%v, want false/false", got.empty, got.byTag)
		}
		if !got.eventsParams.IdsJSON.IsPresent() {
			t.Fatal("IdsJSON should be present")
		}
		if got.eventsParams.ResultLimit != -1 {
			t.Errorf("ResultLimit = %d, want -1", got.eventsParams.ResultLimit)
		}
	})

	t.Run("tag without ids dispatches to QueryEventsByTag with sorted primary tag", func(t *testing.T) {
		t.Parallel()
		got, err := prepareEventQuery(nostr.Filter{
			Authors: []nostr.PubKey{pk1},
			Kinds:   []nostr.Kind{6, 7},
			Tags: nostr.TagMap{
				"p": {pk1.Hex()},
				"e": {id1.Hex(), id2.Hex()},
			},
			Limit: 50,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.empty || !got.byTag {
			t.Fatalf("got empty=%v byTag=%v, want false/true", got.empty, got.byTag)
		}
		if got.byTagParams.TagName != "e" {
			t.Errorf("TagName = %q, want %q", got.byTagParams.TagName, "e")
		}
		if !got.byTagParams.AuthorsJSON.IsPresent() || !got.byTagParams.KindsJSON.IsPresent() {
			t.Errorf("AuthorsJSON/KindsJSON should be present")
		}
		if got.byTagParams.ResultLimit != 50 {
			t.Errorf("ResultLimit = %d, want 50", got.byTagParams.ResultLimit)
		}
		if got.byTagParams.ExtraTags.Encode() == "" {
			t.Errorf("ExtraTags should contain remaining tag predicate")
		}
	})

	t.Run("ids plus tag dispatches to QueryEvents", func(t *testing.T) {
		t.Parallel()
		got, err := prepareEventQuery(nostr.Filter{
			IDs:  []nostr.ID{id1},
			Tags: nostr.TagMap{"e": {id2.Hex()}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.empty || got.byTag {
			t.Fatalf("got empty=%v byTag=%v, want false/false", got.empty, got.byTag)
		}
		if !got.eventsParams.IdsJSON.IsPresent() || got.eventsParams.ExtraTags.Encode() == "" {
			t.Errorf("expected IdsJSON present and ExtraTags non-empty")
		}
	})
}

func explainQueryPlan(t *testing.T, s *SQLiteStore, filter nostr.Filter) string {
	t.Helper()
	plan, err := s.explainEventQuery(t.Context(), filter)
	if err != nil {
		t.Fatalf("explainEventQuery failed: %v", err)
	}
	return plan
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
			plan := explainQueryPlan(t, s, tt.filter)
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
