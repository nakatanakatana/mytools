package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

type purgeStore interface {
	EventIDsByAuthor(context.Context, string) ([]string, error)
	EnqueuePurge(context.Context, store.PurgeRequest) error
}

func EnqueuePurge(ctx context.Context, durable purgeStore, pubkey nostr.PubKey, deletion nostr.Event, limit int64) error {
	if durable == nil || deletion.Kind != 5 || deletion.PubKey != pubkey || !deletion.CheckID() || !deletion.VerifySignature() {
		return errors.New("invalid signed purge deletion event")
	}
	known, err := durable.EventIDsByAuthor(ctx, pubkey.Hex())
	if err != nil {
		return err
	}
	knownSet := make(map[string]struct{}, len(known))
	for _, id := range known {
		knownSet[id] = struct{}{}
	}
	tagged := make(map[string]struct{})
	for _, tag := range deletion.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			tagged[tag[1]] = struct{}{}
		}
	}
	if len(tagged) != len(knownSet) {
		return errors.New("purge deletion does not cover known events")
	}
	for id := range knownSet {
		if _, ok := tagged[id]; !ok {
			return errors.New("purge deletion does not cover known events")
		}
	}
	payload, err := json.Marshal(deletion)
	if err != nil {
		return err
	}
	return durable.EnqueuePurge(ctx, store.PurgeRequest{PubKey: pubkey.Hex(), Payload: string(payload), AvailableAt: time.Now(), Limit: limit})
}
