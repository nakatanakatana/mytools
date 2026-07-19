package bluesky

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	bridgeoauth "github.com/nakatanakatana/mytools/cmd/nostr-bridge/oauth"
)

func TestReconcileDeduplicatesConfiguredFollowsAndLists(t *testing.T) {
	const selectedOne = "at://did:plc:owner/app.bsky.graph.list/one"
	const selectedTwo = "at://did:plc:owner/app.bsky.graph.list/two"
	const excluded = "at://did:plc:owner/app.bsky.graph.list/excluded"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/app.bsky.graph.getFollows":
			_ = json.NewEncoder(w).Encode(map[string]any{"follows": []any{map[string]any{"did": "did:plc:follow"}, map[string]any{"did": "did:plc:shared"}, map[string]any{"did": "did:plc:follow"}}})
		case "/xrpc/app.bsky.graph.getList":
			switch r.URL.Query().Get("list") {
			case selectedOne:
				_ = json.NewEncoder(w).Encode(map[string]any{"list": map[string]any{"uri": selectedOne, "name": "One"}, "items": []any{map[string]any{"subject": map[string]any{"did": "did:plc:shared"}}, map[string]any{"subject": map[string]any{"did": "did:plc:list-one"}}}})
			case selectedTwo:
				_ = json.NewEncoder(w).Encode(map[string]any{"list": map[string]any{"uri": selectedTwo, "name": "Two"}, "items": []any{map[string]any{"subject": map[string]any{"did": "did:plc:list-two"}}, map[string]any{"subject": map[string]any{"did": "did:plc:list-two"}}}})
			case excluded:
				t.Fatal("reconciliation fetched an unspecified list")
			default:
				t.Fatalf("unexpected list %q", r.URL.Query().Get("list"))
			}
		default:
			t.Fatalf("unexpected request %s", r.URL)
		}
	}))
	defer server.Close()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientOptions{HTTPClient: server.Client(), BaseURL: server.URL, Token: bridgeoauth.Token{AccessToken: "access-token", DPoPKey: key}, AccountDID: "did:plc:owner"})
	if err != nil {
		t.Fatal(err)
	}
	targets, err := NewReconciler(client, []string{selectedOne, selectedTwo}).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertDIDs(t, targets.Follows, "did:plc:follow", "did:plc:shared")
	assertDIDs(t, targets.Lists[selectedOne], "did:plc:shared", "did:plc:list-one")
	assertDIDs(t, targets.Lists[selectedTwo], "did:plc:list-two")
	assertDIDs(t, targets.Union, "did:plc:follow", "did:plc:shared", "did:plc:list-one", "did:plc:list-two")
}

func TestReconcileTargetUnionFollowListAndSharedLifecycle(t *testing.T) {
	listURI := "at://owner/list/selected"
	source := targetSource{follows: []Actor{{DID: "did:follow"}, {DID: "did:both"}}, lists: map[string]List{listURI: {URI: listURI, Members: []Actor{{DID: "did:list"}, {DID: "did:both"}}}}}
	targets, err := NewReconciler(source, []string{listURI}).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertDIDs(t, targets.Follows, "did:follow", "did:both")
	assertDIDs(t, targets.Lists[listURI], "did:list", "did:both")
	assertDIDs(t, targets.Union, "did:follow", "did:list", "did:both")
}

type targetSource struct {
	follows []Actor
	lists   map[string]List
}

func (s targetSource) Timeline(context.Context, string, int) (Page, error) { return Page{}, nil }
func (s targetSource) Follows(context.Context) ([]Actor, error)            { return s.follows, nil }
func (s targetSource) List(_ context.Context, uri string) (List, error)    { return s.lists[uri], nil }
func (targetSource) Profile(context.Context, string) (Profile, error)      { return Profile{}, nil }

func assertDIDs(t *testing.T, got DIDSet, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("DID count = %d, want %d: %#v", len(got), len(want), got)
	}
	for _, did := range want {
		if !got.Has(did) {
			t.Fatalf("missing %q from %#v", did, got)
		}
	}
}
