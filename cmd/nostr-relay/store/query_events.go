package store

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"slices"

	"fiatjaf.com/nostr"
	"github.com/moznion/go-sqletch"
	"github.com/moznion/go-sqletch/runtime"
	storesqletch "github.com/nakatanakatana/mytools/cmd/nostr-relay/store/sqletch"
)

type preparedEventQuery struct {
	empty        bool
	byTag        bool
	eventsParams storesqletch.QueryEventsParams
	byTagParams  storesqletch.QueryEventsByTagParams
}

func (s *SQLiteStore) QueryEvents(ctx context.Context, filter nostr.Filter) (iter.Seq[nostr.Event], error) {
	prepared, err := prepareEventQuery(filter)
	if err != nil {
		return nil, err
	}
	if prepared.empty {
		return func(func(nostr.Event) bool) {}, nil
	}

	var encodedEvents []string
	if prepared.byTag {
		rows, err := s.eventQueries.QueryEventsByTag(ctx, prepared.byTagParams)
		if err != nil {
			return nil, err
		}
		encodedEvents = make([]string, len(rows))
		for i, row := range rows {
			encodedEvents[i] = row.EventJSON
		}
	} else {
		rows, err := s.eventQueries.QueryEvents(ctx, prepared.eventsParams)
		if err != nil {
			return nil, err
		}
		encodedEvents = make([]string, len(rows))
		for i, row := range rows {
			encodedEvents[i] = row.EventJSON
		}
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

func (s *SQLiteStore) explainEventQuery(ctx context.Context, filter nostr.Filter) (string, error) {
	prepared, err := prepareEventQuery(filter)
	if err != nil {
		return "", err
	}
	if prepared.empty {
		return "", nil
	}
	if prepared.byTag {
		plan, err := s.eventQueries.ExplainQueryEventsByTag(ctx, prepared.byTagParams, runtime.ExplainOptions{})
		if err != nil {
			return "", err
		}
		return plan.Output, nil
	}
	plan, err := s.eventQueries.ExplainQueryEvents(ctx, prepared.eventsParams, runtime.ExplainOptions{})
	if err != nil {
		return "", err
	}
	return plan.Output, nil
}

func prepareEventQuery(filter nostr.Filter) (preparedEventQuery, error) {
	if filter.LimitZero ||
		(filter.IDs != nil && len(filter.IDs) == 0) ||
		(filter.Authors != nil && len(filter.Authors) == 0) ||
		(filter.Kinds != nil && len(filter.Kinds) == 0) ||
		(filter.Since != 0 && filter.Until != 0 && filter.Since > filter.Until) {
		return preparedEventQuery{empty: true}, nil
	}

	tagNames := slices.Sorted(maps.Keys(filter.Tags))
	for _, tagName := range tagNames {
		if len(filter.Tags[tagName]) == 0 {
			return preparedEventQuery{empty: true}, nil
		}
	}

	var authorsJSON sqletch.Omittable[string]
	if len(filter.Authors) > 0 {
		encoded, err := json.Marshal(filter.Authors)
		if err != nil {
			return preparedEventQuery{}, err
		}
		authorsJSON = sqletch.Present(string(encoded))
	}

	var kindsJSON sqletch.Omittable[string]
	if len(filter.Kinds) > 0 {
		encoded, err := json.Marshal(filter.Kinds)
		if err != nil {
			return preparedEventQuery{}, err
		}
		kindsJSON = sqletch.Present(string(encoded))
	}

	var since sqletch.Omittable[int64]
	if filter.Since != 0 {
		since = sqletch.Present(int64(filter.Since))
	}

	var until sqletch.Omittable[int64]
	if filter.Until != 0 {
		until = sqletch.Present(int64(filter.Until))
	}

	resultLimit := int64(-1)
	if filter.Limit > 0 {
		resultLimit = int64(filter.Limit)
	}

	if len(filter.IDs) == 0 && len(tagNames) > 0 {
		firstTag := tagNames[0]
		firstValues, err := json.Marshal(filter.Tags[firstTag])
		if err != nil {
			return preparedEventQuery{}, err
		}
		extraTags, err := buildExtraTagTree(tagNames[1:], filter.Tags, storesqletch.QueryEventsByTagTagMatch)
		if err != nil {
			return preparedEventQuery{}, err
		}
		return preparedEventQuery{
			byTag: true,
			byTagParams: storesqletch.QueryEventsByTagParams{
				TagName:       firstTag,
				TagValuesJSON: string(firstValues),
				AuthorsJSON:   authorsJSON,
				KindsJSON:     kindsJSON,
				Since:         since,
				Until:         until,
				ResultLimit:   resultLimit,
				ExtraTags:     extraTags,
			},
		}, nil
	}

	var idsJSON sqletch.Omittable[string]
	if len(filter.IDs) > 0 {
		encoded, err := json.Marshal(filter.IDs)
		if err != nil {
			return preparedEventQuery{}, err
		}
		idsJSON = sqletch.Present(string(encoded))
	}

	extraTags, err := buildExtraTagTree(tagNames, filter.Tags, storesqletch.QueryEventsTagMatch)
	if err != nil {
		return preparedEventQuery{}, err
	}

	return preparedEventQuery{
		eventsParams: storesqletch.QueryEventsParams{
			IdsJSON:     idsJSON,
			AuthorsJSON: authorsJSON,
			KindsJSON:   kindsJSON,
			Since:       since,
			Until:       until,
			ResultLimit: resultLimit,
			ExtraTags:   extraTags,
		},
	}, nil
}

func buildExtraTagTree(tagNames []string, tags nostr.TagMap, leafFn func(string, string) runtime.Tree) (runtime.Tree, error) {
	if len(tagNames) == 0 {
		return runtime.Tree{}, nil
	}
	leaves := make([]runtime.Tree, 0, len(tagNames))
	for _, tagName := range tagNames {
		encoded, err := json.Marshal(tags[tagName])
		if err != nil {
			return runtime.Tree{}, err
		}
		leaves = append(leaves, leafFn(tagName, string(encoded)))
	}
	if len(leaves) == 1 {
		return leaves[0], nil
	}
	return storesqletch.And(leaves...), nil
}
