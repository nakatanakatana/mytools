package nostrmap

import (
	"testing"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
)

func TestDeriveActorKeySeparatesProviders(t *testing.T) {
	a, err := DeriveActorKey(testSeed, source.ActorIdentity{Provider: "bluesky", ID: "same"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := DeriveActorKey(testSeed, source.ActorIdentity{Provider: "mastodon", ID: "same"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Public() == b.Public() {
		t.Fatal("provider identities collided")
	}
}

func TestDeriveActorKeyRejectsBlankIdentityParts(t *testing.T) {
	for _, identity := range []source.ActorIdentity{
		{ID: "did:plc:alice"},
		{Provider: "bluesky"},
		{Provider: " \t", ID: "did:plc:alice"},
		{Provider: "bluesky", ID: " \t"},
	} {
		if _, err := DeriveActorKey(testSeed, identity); err == nil {
			t.Errorf("DeriveActorKey(%#v) succeeded", identity)
		}
	}
}

func TestDeriveKeyIsStableAndScopedToDID(t *testing.T) {
	seed := []byte("01234567890123456789012345678901")

	first, err := DeriveKey(seed, "did:plc:alice")
	if err != nil {
		t.Fatal(err)
	}
	second, err := DeriveKey(seed, "did:plc:alice")
	if err != nil {
		t.Fatal(err)
	}
	other, err := DeriveKey(seed, "did:plc:bob")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same seed and DID derived different keys: %x != %x", first, second)
	}
	if first == other {
		t.Fatal("different DIDs derived the same key")
	}
	if first.Public() == nostr.ZeroPK {
		t.Fatal("derived key is not a valid Nostr secret key")
	}
}
