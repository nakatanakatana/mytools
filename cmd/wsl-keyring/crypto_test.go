package main

import (
	"bytes"
	"testing"
)

func TestGenerateDHKeypair(t *testing.T) {
	kp, err := GenerateDHKeypair()
	if err != nil {
		t.Fatalf("GenerateDHKeypair() error: %v", err)
	}
	if kp.Private == nil || kp.Public == nil {
		t.Fatal("keypair has nil values")
	}
	// Public key should be 128 bytes (1024-bit)
	pubBytes := BigIntToBytes(kp.Public)
	if len(pubBytes) != 128 {
		t.Errorf("public key length = %d, want 128", len(pubBytes))
	}
}

func TestDHKeyExchange(t *testing.T) {
	// Simulate client and server key exchange
	serverKP, err := GenerateDHKeypair()
	if err != nil {
		t.Fatalf("server GenerateDHKeypair() error: %v", err)
	}
	clientKP, err := GenerateDHKeypair()
	if err != nil {
		t.Fatalf("client GenerateDHKeypair() error: %v", err)
	}

	serverShared := ComputeSharedSecret(BigIntToBytes(clientKP.Public), serverKP.Private)
	clientShared := ComputeSharedSecret(BigIntToBytes(serverKP.Public), clientKP.Private)

	if !bytes.Equal(serverShared, clientShared) {
		t.Error("shared secrets do not match")
	}
}

func TestDeriveAESKey(t *testing.T) {
	sharedSecret := make([]byte, 128)
	for i := range sharedSecret {
		sharedSecret[i] = byte(i)
	}

	key, err := DeriveAESKey(sharedSecret)
	if err != nil {
		t.Fatalf("DeriveAESKey() error: %v", err)
	}
	if len(key) != 16 {
		t.Errorf("AES key length = %d, want 16", len(key))
	}

	// Deterministic: same input → same output
	key2, _ := DeriveAESKey(sharedSecret)
	if !bytes.Equal(key, key2) {
		t.Error("DeriveAESKey is not deterministic")
	}
}

func TestPKCS7PadUnpad(t *testing.T) {
	cases := []struct {
		name      string
		plaintext []byte
	}{
		{"empty", []byte{}},
		{"short", []byte("hello")},
		{"exact block", []byte("0123456789abcdef")},
		{"two blocks", []byte("0123456789abcdef0123456789abcdef")},
		{"binary", []byte{0x00, 0x01, 0x02, 0xff}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			padded := PKCS7Pad(tc.plaintext, 16)
			if len(padded)%16 != 0 {
				t.Errorf("padded length %d is not multiple of 16", len(padded))
			}

			unpadded, err := PKCS7Unpad(padded)
			if err != nil {
				t.Fatalf("PKCS7Unpad() error: %v", err)
			}
			if !bytes.Equal(unpadded, tc.plaintext) {
				t.Errorf("roundtrip failed: got %x, want %x", unpadded, tc.plaintext)
			}
		})
	}
}

func TestAESCBCRoundtrip(t *testing.T) {
	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(i)
	}

	plaintexts := [][]byte{
		[]byte("hello world"),
		[]byte("secret password 123"),
		[]byte(""),
		make([]byte, 64),
	}

	for _, plaintext := range plaintexts {
		iv, ciphertext, err := AESCBCEncrypt(key, plaintext)
		if err != nil {
			t.Fatalf("AESCBCEncrypt() error: %v", err)
		}
		if len(iv) != 16 {
			t.Errorf("IV length = %d, want 16", len(iv))
		}

		decrypted, err := AESCBCDecrypt(key, iv, ciphertext)
		if err != nil {
			t.Fatalf("AESCBCDecrypt() error: %v", err)
		}
		if !bytes.Equal(decrypted, plaintext) {
			t.Errorf("roundtrip failed: got %q, want %q", decrypted, plaintext)
		}
	}
}

func TestFullDHExchangeWithAES(t *testing.T) {
	// Simulate full Secret Service session: client+server DH → AES key → encrypt/decrypt
	serverKP, err := GenerateDHKeypair()
	if err != nil {
		t.Fatal(err)
	}
	clientKP, err := GenerateDHKeypair()
	if err != nil {
		t.Fatal(err)
	}

	// Server computes AES key
	serverShared := ComputeSharedSecret(BigIntToBytes(clientKP.Public), serverKP.Private)
	serverAESKey, err := DeriveAESKey(serverShared)
	if err != nil {
		t.Fatal(err)
	}

	// Client computes same AES key
	clientShared := ComputeSharedSecret(BigIntToBytes(serverKP.Public), clientKP.Private)
	clientAESKey, err := DeriveAESKey(clientShared)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(serverAESKey, clientAESKey) {
		t.Fatal("AES keys do not match")
	}

	// Client encrypts
	secret := []byte("my-super-secret-password")
	iv, ciphertext, err := AESCBCEncrypt(clientAESKey, secret)
	if err != nil {
		t.Fatal(err)
	}

	// Server decrypts
	plaintext, err := AESCBCDecrypt(serverAESKey, iv, ciphertext)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(plaintext, secret) {
		t.Errorf("decrypted = %q, want %q", plaintext, secret)
	}
}
