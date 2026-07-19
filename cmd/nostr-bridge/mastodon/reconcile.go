package mastodon

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
)

type ReconcileSource interface {
	Account(context.Context) (Account, error)
	Following(context.Context, string) ([]Account, error)
	Lists(context.Context) ([]List, error)
	ListAccounts(context.Context, string) ([]Account, error)
}

type Reconciler struct {
	source  ReconcileSource
	listIDs []string
}

func NewReconciler(api ReconcileSource, listIDs []string) Reconciler {
	return Reconciler{source: api, listIDs: append([]string(nil), listIDs...)}
}

func (r Reconciler) Reconcile(ctx context.Context) (source.TargetSnapshot, []source.Profile, error) {
	if r.source == nil {
		return source.TargetSnapshot{}, nil, errors.New("invalid Mastodon reconciler configuration")
	}
	owner, err := r.source.Account(ctx)
	if err != nil {
		return source.TargetSnapshot{}, nil, err
	}
	origin := normalizedOrigin(owner.URI)
	if origin == "" {
		return source.TargetSnapshot{}, nil, errors.New("invalid Mastodon account URI")
	}
	follows, err := r.source.Following(ctx, owner.ID)
	if err != nil {
		return source.TargetSnapshot{}, nil, err
	}
	configuredIDs := make([]string, 0, len(r.listIDs))
	seenLists := map[string]struct{}{}
	for _, configuredID := range r.listIDs {
		id := strings.TrimSpace(configuredID)
		if id == "" {
			continue
		}
		if _, duplicate := seenLists[id]; duplicate {
			continue
		}
		seenLists[id] = struct{}{}
		configuredIDs = append(configuredIDs, id)
	}
	snapshot := source.TargetSnapshot{Follows: accountsToSet(follows), Lists: map[string]source.List{}, Union: accountsToSet(follows)}
	profiles := make(map[source.ActorIdentity]source.Profile)
	addProfiles(profiles, []Account{owner})
	addProfiles(profiles, follows)
	if len(configuredIDs) == 0 {
		return snapshot, profileValues(profiles), nil
	}
	lists, err := r.source.Lists(ctx)
	if err != nil {
		return source.TargetSnapshot{}, nil, err
	}
	available := make(map[string]List, len(lists))
	for _, list := range lists {
		available[list.ID] = list
	}
	for _, id := range configuredIDs {
		metadata, exists := available[id]
		if !exists {
			return source.TargetSnapshot{}, nil, errors.New("configured Mastodon list not found")
		}
		members, err := r.source.ListAccounts(ctx, id)
		if err != nil {
			return source.TargetSnapshot{}, nil, err
		}
		set := accountsToSet(members)
		qualifiedID := "mastodon:" + origin + ":" + id
		snapshot.Lists[id] = source.List{ID: qualifiedID, Title: metadata.Title, Members: set}
		for identity := range set {
			snapshot.Union[identity] = struct{}{}
		}
		addProfiles(profiles, members)
	}
	return snapshot, profileValues(profiles), nil
}

func profileValues(profiles map[source.ActorIdentity]source.Profile) []source.Profile {
	resultProfiles := make([]source.Profile, 0, len(profiles))
	for _, profile := range profiles {
		resultProfiles = append(resultProfiles, profile)
	}
	return resultProfiles
}

func accountsToSet(accounts []Account) source.IdentitySet {
	set := make(source.IdentitySet, len(accounts))
	for _, account := range accounts {
		if identity, ok := accountIdentity(account); ok {
			set[identity] = struct{}{}
		}
	}
	return set
}
func addProfiles(target map[source.ActorIdentity]source.Profile, accounts []Account) {
	for _, account := range accounts {
		identity, ok := accountIdentity(account)
		if !ok {
			continue
		}
		target[identity] = source.Profile{Identity: identity, DisplayName: account.DisplayName, Description: HTMLToText(account.Note), AvatarURL: account.Avatar, ProfileURL: account.URL}
	}
}
func accountIdentity(account Account) (source.ActorIdentity, bool) {
	uri := strings.TrimSpace(account.URI)
	if uri == "" {
		return source.ActorIdentity{}, false
	}
	return source.ActorIdentity{Provider: "mastodon", ID: uri}, true
}
func normalizedOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}
