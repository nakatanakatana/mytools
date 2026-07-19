package main

import (
	"context"

	"fiatjaf.com/nostr"
	relaystore "github.com/nakatanakatana/mytools/cmd/nostr-relay/store"
)

func persistEventAndApplyDeletion(ctx context.Context, store relaystore.Store, event nostr.Event) error {
	return store.SaveEventAndApplyDeletion(ctx, event)
}
