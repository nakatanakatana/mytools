package mastodon

import (
	"context"
	"errors"
	"testing"

	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
)

type fakeSource struct {
	account                          Account
	follows                          []Account
	lists                            []List
	listMembers                      map[string][]Account
	accountErr, followsErr, listsErr error
	memberErr                        map[string]error
	listsCalls                       int
}

func (f *fakeSource) Account(context.Context) (Account, error) { return f.account, f.accountErr }
func (f *fakeSource) Following(context.Context, string) ([]Account, error) {
	return f.follows, f.followsErr
}
func (f *fakeSource) Lists(context.Context) ([]List, error) {
	f.listsCalls++
	return f.lists, f.listsErr
}
func (f *fakeSource) ListAccounts(_ context.Context, id string) ([]Account, error) {
	return f.listMembers[id], f.memberErr[id]
}

func TestReconcileUnionsFollowsAndConfiguredListMembers(t *testing.T) {
	api := fakeSource{
		account:     Account{ID: "owner", URI: "https://social.example/users/owner"},
		follows:     []Account{{URI: "https://social.example/users/alice"}},
		lists:       []List{{ID: "7", Title: "read"}, {ID: "8", Title: "ignored"}},
		listMembers: map[string][]Account{"7": {{URI: "https://remote.example/users/bob"}}, "8": {{URI: "https://remote.example/users/mallory"}}},
	}
	snapshot, profiles, err := NewReconciler(&api, []string{"7"}).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertIdentitySet(t, snapshot.Follows, mastodonIdentity("https://social.example/users/alice"))
	assertIdentitySet(t, snapshot.Union, mastodonIdentity("https://social.example/users/alice"), mastodonIdentity("https://remote.example/users/bob"))
	list, ok := snapshot.Lists["7"]
	if !ok || list.ID != "mastodon:https://social.example:7" {
		t.Fatalf("list = %#v", list)
	}
	if _, ok := snapshot.Lists["8"]; ok {
		t.Fatal("unconfigured list synchronized")
	}
	if len(profiles) != 3 {
		t.Fatalf("profiles = %#v", profiles)
	}
}

func TestReconcileSkipsListDiscoveryWhenNoListsConfigured(t *testing.T) {
	api := &fakeSource{account: Account{ID: "owner", URI: "https://social.example/users/owner"}, follows: []Account{{URI: "https://social.example/users/alice"}}, listsErr: errors.New("must not call")}
	snapshot, _, err := NewReconciler(api, []string{"", "  "}).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if api.listsCalls != 0 {
		t.Fatalf("Lists calls = %d", api.listsCalls)
	}
	assertIdentitySet(t, snapshot.Union, mastodonIdentity("https://social.example/users/alice"))
}

func TestReconcileReturnsNoSnapshotOnProviderError(t *testing.T) {
	down := errors.New("down")
	cases := map[string]*fakeSource{
		"account":        {accountErr: down},
		"follows":        {account: Account{ID: "owner", URI: "https://social.example/users/owner"}, followsErr: down},
		"list discovery": {account: Account{ID: "owner", URI: "https://social.example/users/owner"}, listsErr: down},
		"list members":   {account: Account{ID: "owner", URI: "https://social.example/users/owner"}, lists: []List{{ID: "7"}}, memberErr: map[string]error{"7": down}},
	}
	for name, api := range cases {
		t.Run(name, func(t *testing.T) {
			configured := []string{"7"}
			if name == "account" || name == "follows" {
				configured = nil
			}
			snapshot, profiles, err := NewReconciler(api, configured).Reconcile(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			if snapshot.Follows != nil || snapshot.Lists != nil || snapshot.Union != nil || profiles != nil {
				t.Fatalf("partial coordinator-facing result: %#v %#v", snapshot, profiles)
			}
		})
	}
}

func mastodonIdentity(uri string) source.ActorIdentity {
	return source.ActorIdentity{Provider: "mastodon", ID: uri}
}

func assertIdentitySet(t *testing.T, got source.IdentitySet, want ...source.ActorIdentity) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("set length = %d, want %d: %#v", len(got), len(want), got)
	}
	for _, id := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("missing identity %#v", id)
		}
	}
}
