package bluesky

import (
	"context"
	"strings"

	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
)

// DIDSet is a deduplicated set of Bluesky decentralized identifiers.
type DIDSet map[string]struct{}

// Has reports whether did belongs to the set.
func (set DIDSet) Has(did string) bool {
	_, ok := set[did]
	return ok
}

// Snapshot converts Bluesky-specific target values to provider-neutral identities.
func (targets TargetSet) Snapshot() source.TargetSnapshot {
	snapshot := source.TargetSnapshot{Follows: identitySet(targets.Follows), Union: identitySet(targets.Union), Lists: make(map[string]source.List, len(targets.Lists))}
	for id, members := range targets.Lists {
		metadata := targets.ListMetadata[id]
		listID := metadata.URI
		if listID == "" {
			listID = id
		}
		snapshot.Lists[id] = source.List{ID: listID, Title: metadata.Name, Description: metadata.Description, Members: identitySet(members)}
	}
	return snapshot
}

func identitySet(dids DIDSet) source.IdentitySet {
	set := make(source.IdentitySet, len(dids))
	for did := range dids {
		set[source.ActorIdentity{Provider: "bluesky", ID: did}] = struct{}{}
	}
	return set
}

// TargetSet groups actual follows, each selected list, and the union for a stream subscriber.
type TargetSet struct {
	Follows      DIDSet
	Lists        map[string]DIDSet
	ListMetadata map[string]List
	Union        DIDSet
}

// Reconciler obtains the configured source sets from Bluesky.
type Reconciler struct {
	source   SourceClient
	listURIs []string
}

// NewReconciler creates a reconciliation pass limited to listURIs.
func NewReconciler(source SourceClient, listURIs []string) Reconciler {
	return Reconciler{source: source, listURIs: append([]string(nil), listURIs...)}
}

// Reconcile fetches actual follows and every configured list, then returns DID-deduplicated target sets.
func (r Reconciler) Reconcile(ctx context.Context) (TargetSet, error) {
	follows, err := r.source.Follows(ctx)
	if err != nil {
		return TargetSet{}, err
	}
	targets := TargetSet{Follows: actorsToSet(follows), Lists: make(map[string]DIDSet), ListMetadata: make(map[string]List), Union: actorsToSet(follows)}
	for _, listURI := range r.listURIs {
		if strings.TrimSpace(listURI) == "" {
			continue
		}
		list, err := r.source.List(ctx, listURI)
		if err != nil {
			return TargetSet{}, err
		}
		membersSet := actorsToSet(list.Members)
		targets.Lists[listURI] = membersSet
		if list.URI == "" {
			list.URI = listURI
		}
		targets.ListMetadata[listURI] = list
		for did := range membersSet {
			targets.Union[did] = struct{}{}
		}
	}
	return targets, nil
}

func actorsToSet(actors []Actor) DIDSet {
	set := make(DIDSet, len(actors))
	for _, actor := range actors {
		if did := strings.TrimSpace(actor.DID); did != "" {
			set[did] = struct{}{}
		}
	}
	return set
}
