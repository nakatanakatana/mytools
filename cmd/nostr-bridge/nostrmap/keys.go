// Package nostrmap maps provider-neutral source records to signed Nostr events.
package nostrmap

import (
	"crypto/sha256"
	"fmt"
	"io"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
	"golang.org/x/crypto/hkdf"
)

const keyInfoPrefix = "nostr-bridge/"

// DeriveActorKey deterministically derives an actor's provider-qualified Nostr signing key.
func DeriveActorKey(masterSeed []byte, identity source.ActorIdentity) (nostr.SecretKey, error) {
	if len(masterSeed) == 0 {
		return nostr.SecretKey{}, fmt.Errorf("master seed must not be empty")
	}
	if strings.TrimSpace(identity.Provider) == "" {
		return nostr.SecretKey{}, fmt.Errorf("actor provider must not be empty")
	}
	if strings.TrimSpace(identity.ID) == "" {
		return nostr.SecretKey{}, fmt.Errorf("actor ID must not be empty")
	}

	var key nostr.SecretKey
	info := keyInfoPrefix + identity.Provider + "\x00" + identity.ID
	if _, err := io.ReadFull(hkdf.New(sha256.New, masterSeed, nil, []byte(info)), key[:]); err != nil {
		return nostr.SecretKey{}, fmt.Errorf("derive Nostr key: %w", err)
	}
	if key.Public() == nostr.ZeroPK {
		return nostr.SecretKey{}, fmt.Errorf("derived invalid Nostr key")
	}
	return key, nil
}

// DeriveKey derives the provider-qualified key for a Bluesky DID.
func DeriveKey(masterSeed []byte, did string) (nostr.SecretKey, error) {
	return DeriveActorKey(masterSeed, source.ActorIdentity{Provider: "bluesky", ID: did})
}
