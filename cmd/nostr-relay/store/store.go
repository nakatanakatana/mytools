package store

import (
	"context"
	"iter"
	"time"

	"fiatjaf.com/nostr"
)

type Publisher struct {
	PubKey    nostr.PubKey
	Reason    string
	CreatedAt time.Time
}

type Store interface {
	SaveEvent(context.Context, nostr.Event) error
	SaveEventAndApplyDeletion(context.Context, nostr.Event) error
	QueryEvents(context.Context, nostr.Filter) (iter.Seq[nostr.Event], error)
	DeleteEvent(context.Context, nostr.ID) error
	Event(context.Context, nostr.ID) (nostr.Event, error)
	AllowPublisher(context.Context, Publisher) error
	UnallowPublisher(context.Context, nostr.PubKey) error
	PublisherAllowed(context.Context, nostr.PubKey) (bool, error)
	ListPublishers(context.Context) ([]Publisher, error)
	ConsumeNIP98Event(context.Context, nostr.ID, time.Time, time.Duration) error
}
