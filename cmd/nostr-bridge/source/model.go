// Package source defines provider-neutral values consumed by the Nostr mapper.
package source

import "time"

// ActorIdentity uniquely identifies an actor within a source provider.
type ActorIdentity struct {
	Provider string
	ID       string
}

// IdentitySet is a deduplicated set of provider-qualified actor identities.
type IdentitySet map[ActorIdentity]struct{}

// Attachment describes source-hosted media referenced by a post.
type Attachment struct {
	URL         string
	MIMEType    string
	Description string
	Blurhash    string
	Width       int
	Height      int
}

// Link describes a web resource referenced by a post.
type Link struct {
	URL string
}

// Profile is the source-neutral profile data needed to create a Nostr profile.
type Profile struct {
	Identity    ActorIdentity
	DisplayName string
	Description string
	AvatarURL   string
	ProfileURL  string
}

// Post is the source-neutral post data needed to create a Nostr text note.
type Post struct {
	ID             string
	Author         ActorIdentity
	SourceURL      string
	Text           string
	ReplyToID      string
	ContentWarning string
	CreatedAt      time.Time
	Attachments    []Attachment
	Links          []Link
}

// List is a provider-qualified source list and its members.
type List struct {
	ID          string
	Title       string
	Description string
	Members     IdentitySet
}

// TargetSnapshot groups actual follows, configured lists, and their union.
type TargetSnapshot struct {
	Follows IdentitySet
	Lists   map[string]List
	Union   IdentitySet
}
