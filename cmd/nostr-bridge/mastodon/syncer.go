package mastodon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/nostrmap"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

type TimelinePage struct {
	Statuses  []Status
	NextMaxID string
}

type TimelineAPI interface {
	HomeTimeline(context.Context, string, string, int) (TimelinePage, error)
	ListTimeline(context.Context, string, string, string, int) (TimelinePage, error)
}

type clientTimelineAPI struct{ *Client }

func (c clientTimelineAPI) HomeTimeline(ctx context.Context, since, max string, limit int) (TimelinePage, error) {
	return c.HomeTimelinePage(ctx, since, max, limit)
}
func (c clientTimelineAPI) ListTimeline(ctx context.Context, id, since, max string, limit int) (TimelinePage, error) {
	return c.ListTimelinePage(ctx, id, since, max, limit)
}

// ClientTimelineAPI adapts Client to the finite/gap timeline interface.
func ClientTimelineAPI(client *Client) TimelineAPI { return clientTimelineAPI{client} }

type SyncOptions struct {
	Scope         bridgestore.SourceScope
	API           TimelineAPI
	Store         bridgestore.SyncDeliveryStore
	MasterSeed    []byte
	Targets       func() source.IdentitySet
	ListIDs       []string
	BackfillLimit int
	OutboxLimit   int64
	StreamURL     string
	Connect       StreamConnector
	Sleep         func(context.Context, int) error
	Observer      StreamObserver
	Now           func() time.Time
}

type StreamObserver interface {
	StreamConnected(bool)
	StreamEvent(time.Time)
}

type Syncer struct{ options SyncOptions }

func NewSyncer(options SyncOptions) *Syncer {
	if options.BackfillLimit <= 0 {
		options.BackfillLimit = 100
	}
	if options.OutboxLimit <= 0 {
		options.OutboxLimit = 10000
	}
	if options.Connect == nil {
		options.Connect = dialStream
	}
	if options.Sleep == nil {
		options.Sleep = backoffSleep
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Syncer{options: options}
}
func (s *Syncer) ref(uri string) bridgestore.SourceRef {
	return bridgestore.SourceRef{Scope: s.options.Scope, URI: uri}
}
func aliasCursor(id string) string     { return "mastodon_status_alias:" + id }
func feedCommitted(feed string) string { return "mastodon_feed:" + feed + ":committed" }
func feedCeiling(feed string) string   { return "mastodon_feed:" + feed + ":ceiling" }
func feedNext(feed string) string      { return "mastodon_feed:" + feed + ":next" }

const recoveryFeedIndex = "mastodon_recovery_feed_index"

// Backfill catches up home and configured list timelines, deduplicated by canonical URI.
func (s *Syncer) Backfill(ctx context.Context) error {
	if s.options.API == nil {
		return errors.New("mastodon timeline API is required")
	}
	if s.options.Store == nil {
		return errors.New("mastodon delivery store is required")
	}
	remaining := s.options.BackfillLimit
	feeds := append([]string{"home"}, uniqueStrings(s.options.ListIDs)...)
	rawIndex, err := s.loadCursor(ctx, recoveryFeedIndex)
	if err != nil {
		return err
	}
	start := 0
	if rawIndex != "" {
		start, err = strconv.Atoi(rawIndex)
		if err != nil || start < 0 {
			return errors.New("invalid Mastodon recovery feed index")
		}
		start %= len(feeds)
	}
	for offset := 0; offset < len(feeds); offset++ {
		if remaining <= 0 {
			break
		}
		index := (start + offset) % len(feeds)
		feed := feeds[index]
		used, err := s.recoverFeed(ctx, feed, remaining)
		if err != nil {
			return err
		}
		remaining -= used
		if err := s.options.Store.SaveCursor(ctx, s.options.Scope, recoveryFeedIndex, strconv.Itoa((index+1)%len(feeds))); err != nil {
			return err
		}
	}
	return nil
}

func (s *Syncer) recoverFeed(ctx context.Context, feed string, budget int) (int, error) {
	committed, err := s.loadCursor(ctx, feedCommitted(feed))
	if err != nil {
		return 0, err
	}
	ceiling, err := s.loadCursor(ctx, feedCeiling(feed))
	if err != nil {
		return 0, err
	}
	next, err := s.loadCursor(ctx, feedNext(feed))
	if err != nil {
		return 0, err
	}
	var page TimelinePage
	if feed == "home" {
		page, err = s.options.API.HomeTimeline(ctx, committed, next, budget)
	} else {
		page, err = s.options.API.ListTimeline(ctx, feed, committed, next, budget)
	}
	if err != nil {
		return 0, fmt.Errorf("read Mastodon %s timeline: %w", feed, err)
	}
	if ceiling == "" && len(page.Statuses) > 0 {
		ceiling = page.Statuses[0].ID
		if err := s.options.Store.SaveCursor(ctx, s.options.Scope, feedCeiling(feed), ceiling); err != nil {
			return 0, err
		}
	}
	statuses := append([]Status(nil), page.Statuses...)
	sort.SliceStable(statuses, func(i, j int) bool { return statuses[i].CreatedAt.Before(statuses[j].CreatedAt) })
	seen := map[string]struct{}{}
	for _, status := range statuses {
		if _, ok := seen[status.URI]; ok {
			continue
		}
		seen[status.URI] = struct{}{}
		if !s.isTarget(status) {
			continue
		}
		if err := s.handleStatus(ctx, status); err != nil {
			return 0, err
		}
	}
	if page.NextMaxID != "" {
		if err := s.options.Store.SaveCursor(ctx, s.options.Scope, feedNext(feed), page.NextMaxID); err != nil {
			return 0, err
		}
		return len(page.Statuses), nil
	}
	if ceiling != "" {
		if err := s.options.Store.SaveCursor(ctx, s.options.Scope, feedCommitted(feed), ceiling); err != nil {
			return 0, err
		}
	}
	if err := s.options.Store.SaveCursor(ctx, s.options.Scope, feedCeiling(feed), ""); err != nil {
		return 0, err
	}
	if err := s.options.Store.SaveCursor(ctx, s.options.Scope, feedNext(feed), ""); err != nil {
		return 0, err
	}
	return len(page.Statuses), nil
}
func (s *Syncer) loadCursor(ctx context.Context, name string) (string, error) {
	v, err := s.options.Store.Cursor(ctx, s.options.Scope, name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load Mastodon recovery cursor: %w", err)
	}
	return v, nil
}

type StreamEvent struct {
	Event    string
	Payload  Status
	DeleteID string
}

// HandleEvent applies a decoded Mastodon streaming event idempotently.
func (s *Syncer) HandleEvent(ctx context.Context, event StreamEvent) error {
	switch event.Event {
	case "update":
		return s.handleStatus(ctx, event.Payload)
	case "status.update":
		return s.handleStatus(ctx, event.Payload)
	case "delete":
		return s.deleteStatus(ctx, event.DeleteID)
	default:
		return nil
	}
}

func (s *Syncer) isTarget(status Status) bool {
	if s.options.Targets == nil {
		return true
	}
	_, ok := s.options.Targets()[source.ActorIdentity{Provider: "mastodon", ID: status.Account.URI}]
	return ok
}
func statusOperation(status Status) string {
	b, _ := json.Marshal(status)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *Syncer) handleStatus(ctx context.Context, status Status) error {
	if s.options.Store == nil {
		return errors.New("mastodon delivery store is required")
	}
	if !s.isTarget(status) {
		return nil
	}
	post, ok, err := NormalizeStatus(status)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	canonical := post.ID
	if status.ID != "" {
		if err := s.options.Store.SaveCursor(ctx, s.options.Scope, aliasCursor(status.ID), canonical); err != nil {
			return fmt.Errorf("save Mastodon status alias: %w", err)
		}
		if err := s.options.Store.SaveCursor(ctx, s.options.Scope, aliasCursor(status.ID)+":author", status.Account.URI); err != nil {
			return fmt.Errorf("save Mastodon status author: %w", err)
		}
	}
	if status.InReplyToID != "" {
		alias, e := s.options.Store.Cursor(ctx, s.options.Scope, aliasCursor(status.InReplyToID))
		if e == nil {
			post.ReplyToID = alias
		} else if !errors.Is(e, sql.ErrNoRows) {
			return fmt.Errorf("load Mastodon reply alias: %w", e)
		}
	}
	op := statusOperation(status)
	previous, e := s.options.Store.SourceOperationBySourceURI(ctx, s.ref(canonical))
	if e == nil && previous == op {
		return nil
	}
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return fmt.Errorf("lookup Mastodon operation: %w", e)
	}
	old, mapErr := s.options.Store.EventMappingBySourceURI(ctx, s.ref(canonical))
	if mapErr != nil && !errors.Is(mapErr, sql.ErrNoRows) {
		return fmt.Errorf("lookup Mastodon mapping: %w", mapErr)
	}
	event, err := s.mapPost(ctx, post)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(event)
	now := time.Now()
	mapping := bridgestore.EventMapping{Source: s.ref(canonical), NostrEventID: event.ID.Hex(), SourceKind: "status", AuthorPubKey: event.PubKey.Hex(), UpdatedAt: now.Unix()}
	var cursor *bridgestore.CursorUpdate
	request := bridgestore.OutboxRequest{AggregateKey: event.PubKey.Hex(), Operation: bridgestore.OutboxPublishEvent, PubKey: event.PubKey.Hex(), Payload: string(payload), AvailableAt: now}
	if errors.Is(mapErr, sql.ErrNoRows) {
		return s.options.Store.EnqueueEvent(ctx, bridgestore.EventEnqueueRequest{Mapping: mapping, Event: request, Limit: s.options.OutboxLimit, Cursor: cursor, SourceOperation: op})
	}
	deletion, err := deletionEvent(s.options.MasterSeed, post.Author, old.NostrEventID, "source status replaced")
	if err != nil {
		return err
	}
	deleted, _ := json.Marshal(deletion)
	return s.options.Store.EnqueueUpdate(ctx, bridgestore.UpdateEnqueueRequest{Mapping: mapping, Deletion: bridgestore.OutboxRequest{AggregateKey: event.PubKey.Hex(), Operation: bridgestore.OutboxPublishEvent, PubKey: event.PubKey.Hex(), Payload: string(deleted), AvailableAt: now}, Replacement: request, SourceOperation: op, Limit: s.options.OutboxLimit, Cursor: cursor})
}

func (s *Syncer) mapPost(ctx context.Context, post source.Post) (nostr.Event, error) {
	parents := map[string]nostr.Event{}
	if post.ReplyToID != "" {
		m, e := s.options.Store.EventMappingBySourceURI(ctx, s.ref(post.ReplyToID))
		if e == nil {
			id, ie := nostr.IDFromHex(m.NostrEventID)
			pk, pe := nostr.PubKeyFromHex(m.AuthorPubKey)
			if ie != nil || pe != nil {
				return nostr.Event{}, errors.New("invalid persisted Mastodon parent mapping")
			}
			parents[post.ReplyToID] = nostr.Event{ID: id, PubKey: pk}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return nostr.Event{}, e
		}
	}
	e, err := nostrmap.PostEvent(s.options.MasterSeed, post, parents)
	if err != nil {
		return nostr.Event{}, fmt.Errorf("map Mastodon status: %w", err)
	}
	return e, nil
}

func deletionEvent(seed []byte, author source.ActorIdentity, eventID, reason string) (nostr.Event, error) {
	key, err := nostrmap.DeriveActorKey(seed, author)
	if err != nil {
		return nostr.Event{}, err
	}
	if _, err := nostr.IDFromHex(eventID); err != nil {
		return nostr.Event{}, errors.New("invalid mapped Mastodon event ID")
	}
	e := nostr.Event{CreatedAt: nostr.Now(), Kind: 5, Tags: nostr.Tags{{"e", eventID}}, Content: reason}
	if err := e.Sign(key); err != nil {
		return nostr.Event{}, err
	}
	return e, nil
}

func (s *Syncer) deleteStatus(ctx context.Context, id string) error {
	canonical, err := s.options.Store.Cursor(ctx, s.options.Scope, aliasCursor(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	mapping, err := s.options.Store.EventMappingBySourceURI(ctx, s.ref(canonical))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	author := source.ActorIdentity{Provider: "mastodon"} // Derive the same author from the persisted pubkey by finding the status mapping is impossible; canonical URI embeds the actor path, so retain a durable author alias on ingest.
	author.ID, err = s.options.Store.Cursor(ctx, s.options.Scope, aliasCursor(id)+":author")
	if err != nil {
		return fmt.Errorf("load Mastodon deletion author: %w", err)
	}
	event, err := deletionEvent(s.options.MasterSeed, author, mapping.NostrEventID, "source status deleted")
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(event)
	return s.options.Store.EnqueueDelete(ctx, bridgestore.DeleteEnqueueRequest{Source: s.ref(canonical), Event: bridgestore.OutboxRequest{AggregateKey: event.PubKey.Hex(), Operation: bridgestore.OutboxPublishEvent, PubKey: event.PubKey.Hex(), Payload: string(payload), AvailableAt: time.Now()}, Limit: s.options.OutboxLimit})
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
