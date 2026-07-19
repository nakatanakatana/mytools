package nostrmap

import (
	"encoding/json"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/bluesky"
)

var testSeed = []byte("01234567890123456789012345678901")

func TestProfileEventMapsBlueskyProfileAndSignsIt(t *testing.T) {
	profile := bluesky.Profile{DID: "did:plc:alice", Handle: "alice.bsky.social", DisplayName: "Alice", Description: "Hello", Avatar: "https://example.com/a.png"}
	event, err := ProfileEvent(testSeed, profile)
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != nostr.KindProfileMetadata || !event.VerifySignature() {
		t.Fatalf("profile event = %#v", event)
	}
	var content map[string]string
	if err := json.Unmarshal([]byte(event.Content), &content); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"name": "Alice", "about": "Hello", "picture": "https://example.com/a.png", "website": "https://bsky.app/profile/alice.bsky.social"} {
		if content[key] != want {
			t.Errorf("profile content[%q] = %q, want %q", key, content[key], want)
		}
	}
}

func TestFollowEventMapsAllFollowedDIDs(t *testing.T) {
	event, err := FollowEvent(testSeed, "did:plc:owner", bluesky.DIDSet{"did:plc:bob": {}, "did:plc:carol": {}})
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != nostr.KindFollowList || !event.VerifySignature() {
		t.Fatalf("follow event = %#v", event)
	}
	assertPTagsForDIDs(t, event.Tags, "did:plc:bob", "did:plc:carol")
}

func TestFollowSetEventMapsMetadataAndMembers(t *testing.T) {
	event, err := FollowSetEvent(testSeed, "did:plc:owner", "at://did:plc:owner/app.bsky.graph.list/friends", "Friends", "People I know", bluesky.DIDSet{"did:plc:bob": {}})
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != nostr.Kind(30000) || !event.VerifySignature() {
		t.Fatalf("follow set event = %#v", event)
	}
	for key, want := range map[string]string{"d": "at://did:plc:owner/app.bsky.graph.list/friends", "title": "Friends", "description": "People I know"} {
		if tag := event.Tags.Find(key); len(tag) != 2 || tag[1] != want {
			t.Errorf("%s tag = %#v, want %q", key, tag, want)
		}
	}
	assertPTagsForDIDs(t, event.Tags, "did:plc:bob")
}

func TestPostEventRetainsTimestampURLAndKnownReplyParent(t *testing.T) {
	parentKey, err := DeriveKey(testSeed, "did:plc:parent")
	if err != nil {
		t.Fatal(err)
	}
	parent := nostr.Event{ID: nostr.ID{1}, PubKey: parentKey.Public()}
	createdAt := time.Unix(1_700_000_000, 0)
	event, err := PostEvent(testSeed, Post{AuthorDID: "did:plc:alice", URI: "at://did:plc:alice/app.bsky.feed.post/3k", Text: "A reply", CreatedAt: createdAt, ReplyToURI: "at://did:plc:parent/app.bsky.feed.post/1"}, map[string]nostr.Event{"at://did:plc:parent/app.bsky.feed.post/1": parent})
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != nostr.KindTextNote || event.CreatedAt != nostr.Timestamp(createdAt.Unix()) || !event.VerifySignature() {
		t.Fatalf("post event = %#v", event)
	}
	if event.Content != "A reply" || event.Tags.FindWithValue("r", "https://bsky.app/profile/did:plc:alice/post/3k") == nil {
		t.Fatalf("post content/tags = %q %#v", event.Content, event.Tags)
	}
	if event.Tags.FindWithValue("e", parent.ID.Hex()) == nil || event.Tags.FindWithValue("p", parent.PubKey.Hex()) == nil {
		t.Fatalf("reply tags = %#v", event.Tags)
	}
}

func TestPostEventOmitsReplyTagsWhenParentIsUnknown(t *testing.T) {
	event, err := PostEvent(testSeed, Post{AuthorDID: "did:plc:alice", URI: "at://did:plc:alice/app.bsky.feed.post/3k", CreatedAt: time.Unix(1, 0), ReplyToURI: "at://did:plc:missing/app.bsky.feed.post/1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if event.Tags.Has("e") || event.Tags.Has("p") {
		t.Fatalf("unknown reply parent produced reply tags: %#v", event.Tags)
	}
}

func assertPTagsForDIDs(t *testing.T, tags nostr.Tags, dids ...string) {
	t.Helper()
	for _, did := range dids {
		key, err := DeriveKey(testSeed, did)
		if err != nil {
			t.Fatal(err)
		}
		if tags.FindWithValue("p", key.Public().Hex()) == nil {
			t.Errorf("p tag for %s missing from %#v", did, tags)
		}
	}
}
