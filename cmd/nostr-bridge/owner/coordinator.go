// Package owner coordinates provider snapshots into events owned by the bridge.
package owner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/nostrmap"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

type Options struct {
	MasterSeed                                   []byte
	OwnerID, OwnerName, OwnerAbout, OwnerPicture string
	Store                                        store.ReconciliationStore
	OutboxLimit                                  int64
	EnabledScopes                                []store.SourceScope
}

type Coordinator struct {
	mu        sync.Mutex
	options   Options
	snapshots map[store.SourceScope]source.TargetSnapshot
	hydrated  bool
}

func New(options Options) *Coordinator {
	return &Coordinator{options: options, snapshots: make(map[store.SourceScope]source.TargetSnapshot)}
}

func (c *Coordinator) Reconcile(ctx context.Context, scope store.SourceScope, snapshot source.TargetSnapshot, profiles []source.Profile) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.hydrate(ctx); err != nil {
		return err
	}
	candidate := make(map[store.SourceScope]source.TargetSnapshot, len(c.snapshots)+1)
	for key, value := range c.snapshots {
		candidate[key] = value
	}
	candidate[scope] = snapshot
	providerEvents, err := c.providerEvents(scope, snapshot, profiles)
	if err != nil {
		return err
	}
	ownerEvents, err := c.ownerEvents(candidate)
	if err != nil {
		return err
	}
	targets := make([]string, 0, len(snapshot.Union))
	for identity := range snapshot.Union {
		targets = append(targets, identity.ID)
	}
	sort.Strings(targets)
	ownerScope := store.SourceScope{Provider: "bridge-owner", Account: c.options.OwnerID}
	events := append(providerEvents, ownerEvents...)
	if err := c.options.Store.ReconcileBatch(ctx, store.ReconciliationBatchRequest{TargetScope: scope, Targets: targets, EventScopes: []store.SourceScope{scope, ownerScope}, Events: events, Limit: c.options.OutboxLimit}); err != nil {
		return err
	}
	c.snapshots = candidate
	return nil
}

func (c *Coordinator) hydrate(ctx context.Context) error {
	if c.hydrated {
		return nil
	}
	hydrated := make(map[store.SourceScope]source.TargetSnapshot, len(c.options.EnabledScopes))
	for _, scope := range c.options.EnabledScopes {
		targets, err := c.options.Store.SyncTargets(ctx, scope)
		if err != nil {
			return fmt.Errorf("hydrate %s target snapshot: %w", scope.Provider, err)
		}
		union := make(source.IdentitySet, len(targets))
		for _, target := range targets {
			union[source.ActorIdentity{Provider: scope.Provider, ID: target}] = struct{}{}
		}
		hydrated[scope] = source.TargetSnapshot{Union: union}
	}
	c.snapshots = hydrated
	c.hydrated = true
	return nil
}

func (c *Coordinator) providerEvents(scope store.SourceScope, snapshot source.TargetSnapshot, profiles []source.Profile) ([]store.EventEnqueueRequest, error) {
	owner := source.ActorIdentity{Provider: "bridge-owner", ID: c.options.OwnerID}
	requests := make([]store.EventEnqueueRequest, 0, len(profiles)+len(snapshot.Lists))
	for _, profile := range profiles {
		event, err := nostrmap.ProfileEvent(c.options.MasterSeed, profile)
		if err != nil {
			return nil, err
		}
		requests = append(requests, eventRequest(scope, "profile/"+profile.Identity.Provider+":"+profile.Identity.ID, event))
	}
	for id, list := range snapshot.Lists {
		if list.ID == "" {
			list.ID = id
		}
		list.ID = scope.Provider + ":" + list.ID
		event, err := nostrmap.FollowSetEvent(c.options.MasterSeed, owner, list)
		if err != nil {
			return nil, err
		}
		requests = append(requests, eventRequest(scope, "list/"+list.ID, event))
	}
	return requests, nil
}

func (c *Coordinator) ownerEvents(snapshots map[store.SourceScope]source.TargetSnapshot) ([]store.EventEnqueueRequest, error) {
	owner := source.ActorIdentity{Provider: "bridge-owner", ID: c.options.OwnerID}
	ownerScope := store.SourceScope{Provider: "bridge-owner", Account: c.options.OwnerID}
	all := make(source.IdentitySet)
	requests := make([]store.EventEnqueueRequest, 0, 2)
	ownerEvent, err := nostrmap.ProfileEvent(c.options.MasterSeed, source.Profile{Identity: owner, DisplayName: c.options.OwnerName, Description: c.options.OwnerAbout, AvatarURL: c.options.OwnerPicture})
	if err != nil {
		return nil, err
	}
	requests = append(requests, eventRequest(ownerScope, "owner/profile", ownerEvent))
	for _, snapshot := range snapshots {
		for identity := range snapshot.Union {
			all[identity] = struct{}{}
		}
	}
	follow, err := nostrmap.FollowEvent(c.options.MasterSeed, owner, all)
	if err != nil {
		return nil, err
	}
	requests = append(requests, eventRequest(ownerScope, "owner/follows", follow))
	return requests, nil
}

func eventRequest(scope store.SourceScope, uri string, event nostr.Event) store.EventEnqueueRequest {
	payload, _ := event.MarshalJSON()
	now := time.Now()
	identityPayload, _ := json.Marshal(struct {
		PubKey  string     `json:"pubkey"`
		Kind    nostr.Kind `json:"kind"`
		Tags    nostr.Tags `json:"tags"`
		Content string     `json:"content"`
	}{event.PubKey.Hex(), event.Kind, event.Tags, event.Content})
	return store.EventEnqueueRequest{Mapping: store.EventMapping{Source: store.SourceRef{Scope: scope, URI: uri}, NostrEventID: event.ID.Hex(), SourceKind: "reconciliation", AuthorPubKey: event.PubKey.Hex(), UpdatedAt: now.Unix()}, Event: store.OutboxRequest{AggregateKey: event.PubKey.Hex(), Operation: store.OutboxPublishEvent, PubKey: event.PubKey.Hex(), Payload: string(payload), AvailableAt: now}, SourceOperation: fmt.Sprintf("sha256:%x", sha256.Sum256(identityPayload))}
}
