package secretbox

import (
	"bytes"
	"testing"
)

func TestBoxSealUsesRandomAuthenticatedEncryption(t *testing.T) {
	box, err := New(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	one, err := box.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	two, err := box.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(one, two) {
		t.Fatal("ciphertexts are equal")
	}
	plain, err := box.Open(one)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "secret" {
		t.Fatalf("plaintext = %q", plain)
	}
	one[len(one)-1] ^= 1
	if _, err := box.Open(one); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestNewRejectsNonAES256Key(t *testing.T) {
	if _, err := New(make([]byte, 16)); err == nil {
		t.Fatal("accepted 16-byte key")
	}
}
