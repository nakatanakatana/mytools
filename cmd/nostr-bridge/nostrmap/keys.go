// Package nostrmap maps Bluesky records to signed Nostr events.
package nostrmap

import (
	"crypto/sha256"
	"fmt"
	"io"
	"strings"

	"fiatjaf.com/nostr"
	"golang.org/x/crypto/hkdf"
)

const keyInfoPrefix = "nostr-bridge/bluesky/"

// DeriveKey deterministically derives the Nostr signing key for a Bluesky DID.
func DeriveKey(masterSeed []byte, did string) (nostr.SecretKey, error) {
	if len(masterSeed) == 0 {
		return nostr.SecretKey{}, fmt.Errorf("master seed must not be empty")
	}
	if strings.TrimSpace(did) == "" {
		return nostr.SecretKey{}, fmt.Errorf("DID must not be empty")
	}

	var key nostr.SecretKey
	if _, err := io.ReadFull(hkdf.New(sha256.New, masterSeed, nil, []byte(keyInfoPrefix+did)), key[:]); err != nil {
		return nostr.SecretKey{}, fmt.Errorf("derive Nostr key: %w", err)
	}
	if key.Public() == nostr.ZeroPK {
		return nostr.SecretKey{}, fmt.Errorf("derived invalid Nostr key")
	}
	return key, nil
}
