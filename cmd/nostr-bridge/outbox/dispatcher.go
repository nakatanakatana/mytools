package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/relayclient"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

const maxStoredError = 256

type Dispatcher struct {
	Store           DispatcherStore
	Management      relayclient.ManagementClient
	Publisher       relayclient.Publisher
	BaseBackoff     time.Duration
	MaxBackoff      time.Duration
	LeaseDuration   time.Duration
	Now             func() time.Time
	PollInterval    time.Duration
	DeliveryTimeout time.Duration
	Observer        interface{ RelayDelivered(time.Time) }
}

// DispatcherStore is the delivery worker's minimal durable contract.
type DispatcherStore interface {
	ClaimOutbox(context.Context, time.Time, time.Duration, int) ([]store.OutboxItem, error)
	CompleteOutbox(context.Context, int64, string, time.Time) error
	CompletePublisherRegistration(context.Context, int64, string, string, time.Time) error
	CompletePublisherUnregistration(context.Context, int64, string, time.Time, string) error
	RecoverPublisherRegistration(context.Context, int64, string, string, time.Time) error
	RetryOutbox(context.Context, int64, string, time.Time, time.Time, string) error
}

func (d *Dispatcher) DispatchOne(ctx context.Context) (bool, error) {
	if d == nil || d.Store == nil {
		return false, errors.New("outbox store is required")
	}
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	lease := d.LeaseDuration
	if lease <= 0 {
		lease = time.Minute
	}
	items, err := d.Store.ClaimOutbox(ctx, now, lease, 1)
	if err != nil {
		return false, err
	}
	if len(items) == 0 {
		return false, nil
	}
	item := items[0]
	deliveryTimeout := d.DeliveryTimeout
	if deliveryTimeout <= 0 {
		deliveryTimeout = 15 * time.Second
	}
	deliveryCtx, cancelDelivery := context.WithTimeout(ctx, deliveryTimeout)
	err = d.deliver(deliveryCtx, item)
	cancelDelivery()
	if err != nil {
		var notAllowed *relayclient.PublisherNotAllowedError
		if item.Operation == store.OutboxPublishEvent && errors.As(err, &notAllowed) {
			if recoverErr := d.Store.RecoverPublisherRegistration(ctx, item.ID, item.ClaimToken, item.PubKey, now); recoverErr != nil && !errors.Is(recoverErr, store.ErrClaimLost) {
				return true, recoverErr
			}
			return true, nil
		}
		next := now.Add(d.backoff(item.Attempts))
		if retryErr := d.Store.RetryOutbox(ctx, item.ID, item.ClaimToken, now, next, sanitize(err)); retryErr != nil && !errors.Is(retryErr, store.ErrClaimLost) {
			return true, retryErr
		}
		return true, nil
	}
	switch item.Operation {
	case store.OutboxAllowPublisher:
		err = d.Store.CompletePublisherRegistration(ctx, item.ID, item.ClaimToken, item.PubKey, now)
	case store.OutboxUnallowPublisher:
		err = d.Store.CompletePublisherUnregistration(ctx, item.ID, item.ClaimToken, now, item.PubKey)
	default:
		err = d.Store.CompleteOutbox(ctx, item.ID, item.ClaimToken, now)
	}
	if errors.Is(err, store.ErrClaimLost) {
		return true, nil
	}
	if err == nil && d.Observer != nil {
		d.Observer.RelayDelivered(now)
	}
	return true, err
}

func (d *Dispatcher) deliver(ctx context.Context, item store.OutboxItem) error {
	pubkey, err := nostr.PubKeyFromHex(item.PubKey)
	if err != nil {
		return errors.New("invalid outbox pubkey")
	}
	switch item.Operation {
	case store.OutboxAllowPublisher:
		if d.Management == nil {
			return errors.New("management client is required")
		}
		return d.Management.AllowPubKey(ctx, pubkey, "nostr-bridge publisher")
	case store.OutboxUnallowPublisher:
		if d.Management == nil {
			return errors.New("management client is required")
		}
		return d.Management.UnallowPubKey(ctx, pubkey, "nostr-bridge publisher")
	case store.OutboxPublishEvent:
		if d.Publisher == nil {
			return errors.New("publisher client is required")
		}
		var event nostr.Event
		if err := json.Unmarshal([]byte(item.Payload), &event); err != nil {
			return errors.New("invalid publish event payload")
		}
		return d.Publisher.Publish(ctx, event)
	default:
		return fmt.Errorf("unsupported outbox operation %q", item.Operation)
	}
}

func (d *Dispatcher) backoff(attempts int) time.Duration {
	base := d.BaseBackoff
	if base <= 0 {
		base = time.Second
	}
	max := d.MaxBackoff
	if max <= 0 {
		max = time.Minute
	}
	delay := base
	for i := 0; i < attempts && delay < max; i++ {
		if delay > max/2 {
			return max
		}
		delay *= 2
	}
	if delay > max {
		return max
	}
	return delay
}

func sanitize(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > maxStoredError {
		s = s[:maxStoredError]
	}
	return s
}

func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		worked, err := d.DispatchOne(ctx)
		if err != nil {
			return err
		}
		if worked {
			continue
		}
		poll := d.PollInterval
		if poll <= 0 {
			poll = 100 * time.Millisecond
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
