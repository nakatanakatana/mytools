// Package syncer synchronizes Bluesky timeline records through a durable outbox.
package syncer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/bluesky"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/nostrmap"
	neutral "github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

const (
	postCollection   = "app.bsky.feed.post"
	repostCollection = "app.bsky.feed.repost"
	jetstreamCursor  = "jetstream_time_us"
)

// Operation is a Bluesky repository operation.
type Operation string

const (
	Create Operation = "create"
	Update Operation = "update"
	Delete Operation = "delete"
)

// Event is the normalized portion of a Jetstream repository event used by Syncer.
type Event struct {
	DID        string
	Collection string
	RKey       string
	CID        string
	Operation  Operation
	TimeUS     int64
	Record     json.RawMessage
}

func (e Event) URI() string { return "at://" + e.DID + "/" + e.Collection + "/" + e.RKey }

// Options configures a Syncer.
type Options struct {
	Source         bluesky.SourceClient
	OutboxStore    bridgestore.SyncDeliveryStore
	OutboxLimit    int64
	MasterSeed     []byte
	Targets        bluesky.DIDSet
	TargetProvider func() bluesky.DIDSet
	TargetUpdates  <-chan struct{}
	BackfillLimit  int
	JetstreamURL   string
	Rewind         time.Duration
	Connect        Connector
	Observer       Observer
}

// Observer receives non-sensitive runtime progress signals.
type Observer interface {
	JetstreamConnected(bool)
	JetstreamEvent(time.Time)
	SyncCompleted(time.Time)
	PendingWork(int)
}

// Syncer performs both the finite initial backfill and the continuing stream sync.
type Syncer struct{ options Options }

func (s *Syncer) targets() bluesky.DIDSet {
	if s.options.TargetProvider != nil {
		return s.options.TargetProvider()
	}
	return s.options.Targets
}

func (s *Syncer) targetFilter() (bluesky.DIDSet, bool) {
	if s.options.TargetProvider != nil {
		return s.options.TargetProvider(), true
	}
	return s.options.Targets, s.options.Targets != nil
}

func New(options Options) *Syncer {
	if options.BackfillLimit <= 0 {
		options.BackfillLimit = 100
	}
	if options.Rewind <= 0 {
		options.Rewind = 3 * time.Second
	}
	if options.Connect == nil {
		options.Connect = dialJetstream
	}
	return &Syncer{options: options}
}

// Backfill pages the authenticated timeline up to the configured record limit.
func (s *Syncer) Backfill(ctx context.Context) error {
	if s.options.Source == nil {
		return errors.New("syncer source is required")
	}
	remaining, cursor := s.options.BackfillLimit, ""
	for remaining > 0 {
		page, err := s.options.Source.Timeline(ctx, cursor, remaining)
		if err != nil {
			return fmt.Errorf("read timeline: %w", err)
		}
		for _, post := range page.Posts {
			targets, filtered := s.targetFilter()
			if filtered {
				if _, ok := targets[post.AuthorDID]; !ok {
					continue
				}
			}
			if err := s.publishPost(ctx, post.URI, post.AuthorDID, post.Text, post.CreatedAt, post.ReplyToURI, post.Images, post.Links, 0, ""); err != nil {
				return err
			}
			remaining--
			s.syncCompleted()
			if remaining == 0 {
				return nil
			}
		}
		if page.Cursor == "" || page.Cursor == cursor {
			return nil
		}
		cursor = page.Cursor
	}
	return nil
}

// Handle applies one repository operation. The source URI is the durable idempotency key.
func (s *Syncer) Handle(ctx context.Context, event Event) error {
	if s.options.Observer != nil {
		s.options.Observer.PendingWork(1)
		defer s.options.Observer.PendingWork(-1)
	}
	if event.Collection != postCollection && event.Collection != repostCollection {
		return nil
	}
	if targets, filtered := s.targetFilter(); filtered {
		if _, ok := targets[event.DID]; !ok {
			return nil
		}
	}
	uri := event.URI()
	var err error
	switch event.Operation {
	case Create:
		if s.options.OutboxStore == nil {
			return errors.New("syncer store is required")
		}
		_, err = s.options.OutboxStore.EventMappingBySourceURI(ctx, uri)
		if err == nil {
			if s.options.OutboxStore != nil && event.TimeUS > 0 {
				err = s.options.OutboxStore.SaveCursor(ctx, jetstreamCursor, event.TimeUS)
			}
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lookup source event: %w", err)
		}
		err = s.publishRecord(ctx, event)
	case Update:
		identity := event.updateIdentity()
		var previous string
		var lookupErr error
		if s.options.OutboxStore == nil {
			return errors.New("syncer store is required")
		}
		previous, lookupErr = s.options.OutboxStore.SourceOperationBySourceURI(ctx, uri)
		if lookupErr == nil && previous == identity {
			if s.options.OutboxStore != nil && event.TimeUS > 0 {
				err = s.options.OutboxStore.SaveCursor(ctx, jetstreamCursor, event.TimeUS)
			}
			break
		}
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return fmt.Errorf("lookup source operation: %w", lookupErr)
		}
		err = s.replaceRecord(ctx, event)
	case Delete:
		err = s.deleteSource(ctx, uri, event.DID, event.TimeUS)
	default:
		return fmt.Errorf("unsupported repository operation %q", event.Operation)
	}
	if err != nil {
		return err
	}
	s.syncCompleted()
	return nil
}

func (s *Syncer) syncCompleted() {
	if s.options.Observer != nil {
		s.options.Observer.SyncCompleted(time.Now())
	}
}

func (e Event) updateIdentity() string {
	if e.CID != "" {
		return "cid:" + e.CID
	}
	return fmt.Sprintf("time-us:%d:record:%x", e.TimeUS, sha256.Sum256(e.Record))
}

func (s *Syncer) publishRecord(ctx context.Context, event Event) error {
	var record struct {
		Text      string    `json:"text"`
		CreatedAt time.Time `json:"createdAt"`
		Reply     *struct {
			Parent struct {
				URI string `json:"uri"`
			} `json:"parent"`
		} `json:"reply"`
		Subject struct {
			URI string `json:"uri"`
		} `json:"subject"`
		Embed  imageEmbed      `json:"embed"`
		Facets []bluesky.Facet `json:"facets"`
	}
	if err := json.Unmarshal(event.Record, &record); err != nil {
		return fmt.Errorf("decode %s record: %w", event.Collection, err)
	}
	postURI, replyTo := event.URI(), ""
	if record.Reply != nil {
		replyTo = record.Reply.Parent.URI
	}
	if event.Collection == repostCollection {
		postURI = record.Subject.URI
		if postURI == "" {
			return errors.New("repost record has no subject URI")
		}
	}
	if event.Operation == Update && s.options.OutboxStore != nil {
		return s.enqueueReplacement(ctx, event, postURI, record.Text, record.CreatedAt, replyTo, jetstreamImages(event.DID, record.Embed), bluesky.LinksFromFacetsAndExternal(record.Facets, externalURI(record.Embed)))
	}
	operation := ""
	if event.Operation == Update {
		operation = event.updateIdentity()
	}
	return s.publishPost(ctx, event.URI(), event.DID, record.Text, record.CreatedAt, replyTo, jetstreamImages(event.DID, record.Embed), bluesky.LinksFromFacetsAndExternal(record.Facets, externalURI(record.Embed)), event.TimeUS, operation, postURI)
}

type imageEmbed struct {
	Images []struct {
		Alt   string `json:"alt"`
		Image struct {
			Ref struct {
				Link string `json:"$link"`
			} `json:"ref"`
			MIMEType string `json:"mimeType"`
		} `json:"image"`
		AspectRatio *struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"aspectRatio"`
	} `json:"images"`
	Media    *imageEmbed `json:"media"`
	External struct {
		URI string `json:"uri"`
	} `json:"external"`
}

func externalURI(embed imageEmbed) string {
	if embed.External.URI != "" {
		return embed.External.URI
	}
	if embed.Media != nil {
		return externalURI(*embed.Media)
	}
	return ""
}

func jetstreamImages(did string, embed imageEmbed) []bluesky.Image {
	if embed.Media != nil {
		return jetstreamImages(did, *embed.Media)
	}
	images := make([]bluesky.Image, 0, len(embed.Images))
	for _, source := range embed.Images {
		extension := imageExtension(source.Image.MIMEType)
		if source.Image.Ref.Link == "" || extension == "" {
			continue
		}
		image := bluesky.Image{URL: "https://cdn.bsky.app/img/feed_fullsize/plain/" + did + "/" + source.Image.Ref.Link + "@" + extension, MIMEType: source.Image.MIMEType, Alt: source.Alt}
		if source.AspectRatio != nil {
			image.Width = source.AspectRatio.Width
			image.Height = source.AspectRatio.Height
		}
		images = append(images, image)
	}
	return images
}

func imageExtension(mimeType string) string {
	switch strings.ToLower(mimeType) {
	case "image/jpeg":
		return "jpeg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		return ""
	}
}

func (s *Syncer) publishPost(ctx context.Context, sourceURI, did, text string, createdAt time.Time, replyTo string, images []bluesky.Image, links []bluesky.Link, cursor int64, sourceOperation string, mappedURI ...string) error {
	if s.options.OutboxStore == nil {
		return errors.New("syncer store is required")
	}
	if s.options.OutboxStore != nil {
		if _, err := s.options.OutboxStore.EventMappingBySourceURI(ctx, sourceURI); err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lookup source event: %w", err)
		}
	}
	uri := sourceURI
	if len(mappedURI) > 0 {
		uri = mappedURI[0]
	}
	parents := map[string]nostr.Event{}
	if replyTo != "" {
		parent, lookupErr := s.options.OutboxStore.EventMappingBySourceURI(ctx, replyTo)
		if lookupErr == nil {
			id, idErr := nostr.IDFromHex(parent.NostrEventID)
			pubkey, keyErr := nostr.PubKeyFromHex(parent.AuthorPubKey)
			if idErr != nil || keyErr != nil {
				return errors.New("invalid persisted parent mapping")
			}
			parents[replyTo] = nostr.Event{ID: id, PubKey: pubkey}
		} else if !errors.Is(lookupErr, sql.ErrNoRows) {
			return fmt.Errorf("lookup reply parent: %w", lookupErr)
		}
	}
	sourceURL, err := blueskyPostURL(uri)
	if err != nil {
		return fmt.Errorf("map Bluesky post: %w", err)
	}
	mapped, err := nostrmap.PostEvent(s.options.MasterSeed, neutral.Post{ID: uri, Author: blueskyIdentity(did), SourceURL: sourceURL, Text: text, CreatedAt: createdAt, ReplyToID: replyTo, Attachments: blueskyAttachments(images), Links: blueskyLinks(links)}, parents)
	if err != nil {
		return fmt.Errorf("map Bluesky post: %w", err)
	}
	encoded, err := json.Marshal(mapped)
	if err != nil {
		return fmt.Errorf("encode Nostr event: %w", err)
	}
	return s.enqueueMappedEvent(ctx, sourceURI, mapped, string(encoded), cursor, sourceOperation)
}

func (s *Syncer) enqueueMappedEvent(ctx context.Context, sourceURI string, event nostr.Event, payload string, cursor int64, sourceOperation string) error {
	limit := s.options.OutboxLimit
	if limit <= 0 {
		limit = 10000
	}
	now := time.Now()
	mapping := bridgestore.EventMapping{SourceURI: sourceURI, NostrEventID: event.ID.Hex(), SourceKind: postCollection, AuthorPubKey: event.PubKey.Hex(), UpdatedAt: now.Unix()}
	request := bridgestore.OutboxRequest{AggregateKey: event.PubKey.Hex(), Operation: bridgestore.OutboxPublishEvent, PubKey: event.PubKey.Hex(), Payload: payload, AvailableAt: now}
	enqueue := bridgestore.EventEnqueueRequest{Mapping: mapping, Event: request, Limit: limit, SourceOperation: sourceOperation}
	if cursor > 0 {
		enqueue.Cursor = &bridgestore.CursorUpdate{Name: jetstreamCursor, Value: cursor}
	}
	if err := s.options.OutboxStore.EnqueueEvent(ctx, enqueue); err != nil {
		return fmt.Errorf("save mapped event and enqueue: %w", err)
	}
	return nil
}

func (s *Syncer) replaceRecord(ctx context.Context, event Event) error {
	return s.publishRecord(ctx, event)
}

func (s *Syncer) enqueueReplacement(ctx context.Context, source Event, mappedURI, text string, createdAt time.Time, replyTo string, images []bluesky.Image, links []bluesky.Link) error {
	old, err := s.options.OutboxStore.EventMappingBySourceURI(ctx, source.URI())
	if err != nil {
		return fmt.Errorf("lookup source event: %w", err)
	}
	key, err := nostrmap.DeriveKey(s.options.MasterSeed, source.DID)
	if err != nil {
		return err
	}
	oldID, err := nostr.IDFromHex(old.NostrEventID)
	if err != nil {
		return errors.New("invalid mapped event ID")
	}
	deletion := nostr.Event{CreatedAt: nostr.Now(), Kind: 5, Tags: nostr.Tags{{"e", oldID.Hex()}}, Content: "source record replaced"}
	if err := deletion.Sign(key); err != nil {
		return fmt.Errorf("sign deletion event: %w", err)
	}
	sourceURL, err := blueskyPostURL(mappedURI)
	if err != nil {
		return err
	}
	replacement, err := nostrmap.PostEvent(s.options.MasterSeed, neutral.Post{ID: mappedURI, Author: blueskyIdentity(source.DID), SourceURL: sourceURL, Text: text, CreatedAt: createdAt, ReplyToID: replyTo, Attachments: blueskyAttachments(images), Links: blueskyLinks(links)}, nil)
	if err != nil {
		return err
	}
	deletedPayload, _ := json.Marshal(deletion)
	replacementPayload, _ := json.Marshal(replacement)
	now := time.Now()
	limit := s.options.OutboxLimit
	if limit <= 0 {
		limit = 10000
	}
	request := bridgestore.UpdateEnqueueRequest{Mapping: bridgestore.EventMapping{SourceURI: source.URI(), NostrEventID: replacement.ID.Hex(), SourceKind: postCollection, AuthorPubKey: replacement.PubKey.Hex(), UpdatedAt: now.Unix()}, Deletion: bridgestore.OutboxRequest{AggregateKey: replacement.PubKey.Hex(), Operation: bridgestore.OutboxPublishEvent, PubKey: replacement.PubKey.Hex(), Payload: string(deletedPayload), AvailableAt: now}, Replacement: bridgestore.OutboxRequest{AggregateKey: replacement.PubKey.Hex(), Operation: bridgestore.OutboxPublishEvent, PubKey: replacement.PubKey.Hex(), Payload: string(replacementPayload), AvailableAt: now}, SourceOperation: source.updateIdentity(), Limit: limit}
	if source.TimeUS > 0 {
		request.Cursor = &bridgestore.CursorUpdate{Name: jetstreamCursor, Value: source.TimeUS}
	}
	return s.options.OutboxStore.EnqueueUpdate(ctx, request)
}

func blueskyIdentity(did string) neutral.ActorIdentity {
	return neutral.ActorIdentity{Provider: "bluesky", ID: did}
}

func blueskyAttachments(images []bluesky.Image) []neutral.Attachment {
	attachments := make([]neutral.Attachment, len(images))
	for i, image := range images {
		attachments[i] = neutral.Attachment{URL: image.URL, MIMEType: image.MIMEType, Description: image.Alt, Width: image.Width, Height: image.Height}
	}
	return attachments
}

func blueskyLinks(links []bluesky.Link) []neutral.Link {
	values := make([]neutral.Link, len(links))
	for i, link := range links {
		values[i] = neutral.Link{URL: link.URI}
	}
	return values
}

func blueskyPostURL(uri string) (string, error) {
	const prefix = "at://"
	parts := strings.Split(strings.TrimPrefix(uri, prefix), "/")
	if !strings.HasPrefix(uri, prefix) || len(parts) != 3 || parts[0] == "" || parts[1] != postCollection || parts[2] == "" {
		return "", fmt.Errorf("invalid Bluesky post URI %q", uri)
	}
	return "https://bsky.app/profile/" + parts[0] + "/post/" + parts[2], nil
}

func (s *Syncer) deleteSource(ctx context.Context, uri, did string, cursor int64) error {
	if s.options.OutboxStore != nil {
		persisted, err := s.options.OutboxStore.EventMappingBySourceURI(ctx, uri)
		if errors.Is(err, sql.ErrNoRows) {
			if cursor > 0 {
				return s.options.OutboxStore.SaveCursor(ctx, jetstreamCursor, cursor)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("lookup source event: %w", err)
		}
		key, err := nostrmap.DeriveKey(s.options.MasterSeed, did)
		if err != nil {
			return fmt.Errorf("derive deletion key: %w", err)
		}
		id, err := nostr.IDFromHex(persisted.NostrEventID)
		if err != nil {
			return errors.New("invalid mapped event ID")
		}
		deletion := nostr.Event{CreatedAt: nostr.Now(), Kind: 5, Tags: nostr.Tags{{"e", id.Hex()}}, Content: "source record deleted"}
		if err := deletion.Sign(key); err != nil {
			return fmt.Errorf("sign deletion event: %w", err)
		}
		payload, err := json.Marshal(deletion)
		if err != nil {
			return err
		}
		limit := s.options.OutboxLimit
		if limit <= 0 {
			limit = 10000
		}
		req := bridgestore.DeleteEnqueueRequest{SourceURI: uri, Event: bridgestore.OutboxRequest{AggregateKey: deletion.PubKey.Hex(), Operation: bridgestore.OutboxPublishEvent, PubKey: deletion.PubKey.Hex(), Payload: string(payload), AvailableAt: time.Now()}, Limit: limit}
		if cursor > 0 {
			req.Cursor = &bridgestore.CursorUpdate{Name: jetstreamCursor, Value: cursor}
		}
		return s.options.OutboxStore.EnqueueDelete(ctx, req)
	}
	return errors.New("syncer store is required")
}

// Subscription returns the next target-filtered Jetstream URL, rewinding its durable cursor.
func (s *Syncer) Subscription(ctx context.Context, endpoint string) (*url.URL, error) {
	if s.options.OutboxStore == nil {
		return nil, errors.New("syncer store is required")
	}
	targets := s.targets()
	if len(targets) == 0 {
		return nil, errors.New("Jetstream subscription requires at least one target DID") //nolint:staticcheck // Jetstream is a product name.
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse Jetstream URL: %w", err)
	}
	q := u.Query()
	dids := make([]string, 0, len(targets))
	for did := range targets {
		dids = append(dids, did)
	}
	sort.Strings(dids)
	for _, did := range dids {
		q.Add("wantedDids", did)
	}
	q.Add("wantedCollections", postCollection)
	q.Add("wantedCollections", repostCollection)
	var cursor int64
	cursor, err = s.options.OutboxStore.Cursor(ctx, jetstreamCursor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load Jetstream cursor: %w", err)
	}
	if cursor > 0 {
		cursor -= s.options.Rewind.Microseconds()
		if cursor < 0 {
			cursor = 0
		}
		q.Set("cursor", fmt.Sprint(cursor))
	}
	u.RawQuery = q.Encode()
	return u, nil
}

// Run performs backfill then reconnects Jetstream until its context is cancelled.
func (s *Syncer) Run(ctx context.Context) error {
	if err := s.Backfill(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(s.options.JetstreamURL) == "" {
		return nil
	}
	for ctx.Err() == nil {
		u, err := s.Subscription(ctx, s.options.JetstreamURL)
		if err != nil {
			return err
		}
		connection, err := s.options.Connect(ctx, u.String())
		if err == nil {
			if s.options.Observer != nil {
				s.options.Observer.JetstreamConnected(true)
			}
			changed, consumeErr := s.consumeUntilTargetUpdate(ctx, connection)
			err = consumeErr
			_ = connection.Close()
			if s.options.Observer != nil {
				s.options.Observer.JetstreamConnected(false)
			}
			if changed {
				continue
			}
		}
		if ctx.Err() != nil {
			break
		}
		if err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
	return nil
}

// consumeUntilTargetUpdate closes the active subscription when reconciliation
// changes its server-side DID filter, so the next connection uses the new set.
func (s *Syncer) consumeUntilTargetUpdate(ctx context.Context, connection Connection) (bool, error) {
	if s.options.TargetUpdates == nil {
		return false, s.consume(ctx, connection)
	}
	consumeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	changed := make(chan struct{}, 1)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-consumeCtx.Done():
		case <-s.options.TargetUpdates:
			changed <- struct{}{}
			cancel()
			_ = connection.Close()
		}
	}()
	err := s.consume(consumeCtx, connection)
	cancel()
	<-watchDone
	select {
	case <-changed:
		return true, err
	default:
		return false, err
	}
}
