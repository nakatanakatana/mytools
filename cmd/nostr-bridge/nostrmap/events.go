package nostrmap

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
)

// ProfileEvent creates a signed kind 0 event for a source profile.
func ProfileEvent(masterSeed []byte, profile source.Profile, createdAt nostr.Timestamp) (nostr.Event, error) {
	key, err := DeriveActorKey(masterSeed, profile.Identity)
	if err != nil {
		return nostr.Event{}, err
	}
	content, err := json.Marshal(map[string]string{
		"name":    profile.DisplayName,
		"about":   profile.Description,
		"picture": profile.AvatarURL,
		"website": profile.ProfileURL,
	})
	if err != nil {
		return nostr.Event{}, fmt.Errorf("marshal profile content: %w", err)
	}
	return signedEvent(key, nostr.Event{CreatedAt: createdAt, Kind: nostr.KindProfileMetadata, Content: string(content)})
}

// FollowEvent creates a signed aggregate kind 3 event for the owner's follows.
func FollowEvent(masterSeed []byte, owner source.ActorIdentity, follows source.IdentitySet, createdAt nostr.Timestamp) (nostr.Event, error) {
	key, err := DeriveActorKey(masterSeed, owner)
	if err != nil {
		return nostr.Event{}, err
	}
	tags, err := pTags(masterSeed, follows)
	if err != nil {
		return nostr.Event{}, err
	}
	return signedEvent(key, nostr.Event{CreatedAt: createdAt, Kind: nostr.KindFollowList, Tags: tags})
}

// FollowSetEvent creates a signed kind 30000 follow set for one source list.
func FollowSetEvent(masterSeed []byte, owner source.ActorIdentity, list source.List, createdAt nostr.Timestamp) (nostr.Event, error) {
	key, err := DeriveActorKey(masterSeed, owner)
	if err != nil {
		return nostr.Event{}, err
	}
	pTags, err := pTags(masterSeed, list.Members)
	if err != nil {
		return nostr.Event{}, err
	}
	tags := nostr.Tags{{"d", list.ID}, {"title", list.Title}, {"description", list.Description}}
	tags = append(tags, pTags...)
	return signedEvent(key, nostr.Event{CreatedAt: createdAt, Kind: nostr.Kind(30000), Tags: tags})
}

// PostEvent creates a signed kind 1 event. A reply is linked only if its parent
// has already been mapped and is available in parents by its source ID.
func PostEvent(masterSeed []byte, post source.Post, parents map[string]nostr.Event) (nostr.Event, error) {
	key, err := DeriveActorKey(masterSeed, post.Author)
	if err != nil {
		return nostr.Event{}, err
	}
	tags := nostr.Tags{{"r", post.SourceURL}}
	if post.ContentWarning != "" {
		tags = append(tags, nostr.Tag{"content-warning", post.ContentWarning})
	}
	if parent, ok := parents[post.ReplyToID]; ok && post.ReplyToID != "" {
		tags = append(tags, nostr.Tag{"e", parent.ID.Hex(), "", "reply"}, nostr.Tag{"p", parent.PubKey.Hex()})
	}
	content := post.Text
	seenLinks := make(map[string]struct{}, len(post.Links))
	seenRTags := map[string]struct{}{post.SourceURL: {}}
	appendedLinks := 0
	for _, link := range post.Links {
		if link.URL == "" {
			continue
		}
		if _, ok := seenLinks[link.URL]; ok {
			continue
		}
		seenLinks[link.URL] = struct{}{}
		if _, ok := seenRTags[link.URL]; !ok {
			tags = append(tags, nostr.Tag{"r", link.URL})
			seenRTags[link.URL] = struct{}{}
		}
		if strings.Contains(content, link.URL) {
			continue
		}
		if appendedLinks > 0 {
			content += "\n"
		} else if content != "" {
			content += "\n\n"
		}
		content += link.URL
		appendedLinks++
	}
	seen := make(map[string]struct{}, len(post.Attachments))
	imageCount := 0
	for _, image := range post.Attachments {
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
		if image.Description != "" {
			metadata = append(metadata, "alt "+image.Description)
		}
		if image.Blurhash != "" {
			metadata = append(metadata, "blurhash "+image.Blurhash)
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

func pTags(masterSeed []byte, identities source.IdentitySet) (nostr.Tags, error) {
	values := make([]source.ActorIdentity, 0, len(identities))
	for identity := range identities {
		values = append(values, identity)
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].Provider == values[j].Provider {
			return values[i].ID < values[j].ID
		}
		return values[i].Provider < values[j].Provider
	})
	tags := make(nostr.Tags, 0, len(values))
	for _, identity := range values {
		key, err := DeriveActorKey(masterSeed, identity)
		if err != nil {
			return nil, err
		}
		tags = append(tags, nostr.Tag{"p", key.Public().Hex()})
	}
	return tags, nil
}
