package nostrmap

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/bluesky"
)

// Post is the Bluesky post data needed to create a Nostr text note.
type Post struct {
	AuthorDID  string
	URI        string
	Text       string
	CreatedAt  time.Time
	ReplyToURI string
	Images     []bluesky.Image
}

// ProfileEvent creates a signed kind 0 event for a Bluesky profile.
func ProfileEvent(masterSeed []byte, profile bluesky.Profile) (nostr.Event, error) {
	key, err := DeriveKey(masterSeed, profile.DID)
	if err != nil {
		return nostr.Event{}, err
	}
	content, err := json.Marshal(map[string]string{
		"name":    profile.DisplayName,
		"about":   profile.Description,
		"picture": profile.Avatar,
		"website": "https://bsky.app/profile/" + profile.Handle,
	})
	if err != nil {
		return nostr.Event{}, fmt.Errorf("marshal profile content: %w", err)
	}
	return signedEvent(key, nostr.Event{Kind: nostr.KindProfileMetadata, Content: string(content)})
}

// FollowEvent creates a signed aggregate kind 3 event for the account's follows.
func FollowEvent(masterSeed []byte, did string, follows bluesky.DIDSet) (nostr.Event, error) {
	key, err := DeriveKey(masterSeed, did)
	if err != nil {
		return nostr.Event{}, err
	}
	tags, err := pTags(masterSeed, follows)
	if err != nil {
		return nostr.Event{}, err
	}
	return signedEvent(key, nostr.Event{Kind: nostr.KindFollowList, Tags: tags})
}

// FollowSetEvent creates a signed kind 30000 follow set for one Bluesky list.
func FollowSetEvent(masterSeed []byte, did, identifier, title, description string, members bluesky.DIDSet) (nostr.Event, error) {
	key, err := DeriveKey(masterSeed, did)
	if err != nil {
		return nostr.Event{}, err
	}
	pTags, err := pTags(masterSeed, members)
	if err != nil {
		return nostr.Event{}, err
	}
	tags := nostr.Tags{{"d", identifier}, {"title", title}, {"description", description}}
	tags = append(tags, pTags...)
	return signedEvent(key, nostr.Event{Kind: nostr.Kind(30000), Tags: tags})
}

// PostEvent creates a signed kind 1 event. A reply is linked only if its parent
// has already been mapped and is available in parents by its Bluesky URI.
func PostEvent(masterSeed []byte, post Post, parents map[string]nostr.Event) (nostr.Event, error) {
	key, err := DeriveKey(masterSeed, post.AuthorDID)
	if err != nil {
		return nostr.Event{}, err
	}
	url, err := blueskyPostURL(post.URI)
	if err != nil {
		return nostr.Event{}, err
	}
	tags := nostr.Tags{{"r", url}}
	if parent, ok := parents[post.ReplyToURI]; ok && post.ReplyToURI != "" {
		tags = append(tags, nostr.Tag{"e", parent.ID.Hex(), "", "reply"}, nostr.Tag{"p", parent.PubKey.Hex()})
	}
	content := post.Text
	seen := make(map[string]struct{}, len(post.Images))
	imageCount := 0
	for _, image := range post.Images {
		if image.URL == "" {
			continue
		}
		if _, ok := seen[image.URL]; ok {
			continue
		}
		seen[image.URL] = struct{}{}
		if imageCount > 0 {
			content += "\n"
		} else if content != "" {
			content += "\n\n"
		}
		content += image.URL
		imageCount++
		metadata := nostr.Tag{"imeta", "url " + image.URL}
		if image.MIMEType != "" {
			metadata = append(metadata, "m "+image.MIMEType)
		}
		if image.Alt != "" {
			metadata = append(metadata, "alt "+image.Alt)
		}
		if image.Width > 0 && image.Height > 0 {
			metadata = append(metadata, fmt.Sprintf("dim %dx%d", image.Width, image.Height))
		}
		tags = append(tags, metadata)
	}
	return signedEvent(key, nostr.Event{CreatedAt: nostr.Timestamp(post.CreatedAt.Unix()), Kind: nostr.KindTextNote, Tags: tags, Content: content})
}

func signedEvent(key nostr.SecretKey, event nostr.Event) (nostr.Event, error) {
	if err := event.Sign(key); err != nil {
		return nostr.Event{}, fmt.Errorf("sign Nostr event: %w", err)
	}
	return event, nil
}

func pTags(masterSeed []byte, dids bluesky.DIDSet) (nostr.Tags, error) {
	values := make([]string, 0, len(dids))
	for did := range dids {
		values = append(values, did)
	}
	sort.Strings(values)
	tags := make(nostr.Tags, 0, len(values))
	for _, did := range values {
		key, err := DeriveKey(masterSeed, did)
		if err != nil {
			return nil, err
		}
		tags = append(tags, nostr.Tag{"p", key.Public().Hex()})
	}
	return tags, nil
}

func blueskyPostURL(uri string) (string, error) {
	const prefix = "at://"
	parts := strings.Split(strings.TrimPrefix(uri, prefix), "/")
	if !strings.HasPrefix(uri, prefix) || len(parts) != 3 || parts[0] == "" || parts[1] != "app.bsky.feed.post" || parts[2] == "" {
		return "", fmt.Errorf("invalid Bluesky post URI %q", uri)
	}
	return "https://bsky.app/profile/" + parts[0] + "/post/" + parts[2], nil
}
