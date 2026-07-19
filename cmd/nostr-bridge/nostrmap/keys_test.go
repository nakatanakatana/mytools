package nostrmap

import (
	"testing"

	"fiatjaf.com/nostr"
)

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
