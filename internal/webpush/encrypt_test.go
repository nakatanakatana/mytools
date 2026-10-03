package webpush_test

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/nakatanakatana/mytools/internal/webpush"
)

func TestEncrypt_RoundTrip(t *testing.T) {
	clientPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}

	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatalf("generate auth secret: %v", err)
	}

	sub := webpush.Subscription{
		Endpoint: "https://push.example.com/sub/test-client",
		Keys: webpush.SubscriptionKeys{
			P256dh: base64.RawURLEncoding.EncodeToString(clientPriv.PublicKey().Bytes()),
			Auth:   base64.RawURLEncoding.EncodeToString(authSecret),
		},
	}

	tests := []struct {
		name      string
		plaintext []byte
	}{
		{
			name:      "JSON notification payload",
			plaintext: []byte(`{"title":"New Mention","body":"Hello Nostr!"}`),
		},
		{
			name:      "empty payload",
			plaintext: []byte(""),
		},
		{
			name:      "large payload near max",
			plaintext: bytes.Repeat([]byte("A"), 3993),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encrypted, err := webpush.Encrypt(tc.plaintext, sub)
			if err != nil {
				t.Fatalf("Encrypt failed: %v", err)
			}

			decrypted, err := webpush.DecryptForTest(encrypted, clientPriv, authSecret)
			if err != nil {
				t.Fatalf("DecryptForTest failed: %v", err)
			}

			if !bytes.Equal(decrypted, tc.plaintext) {
				t.Fatalf("decrypted text does not match plaintext: got %q, want %q", decrypted, tc.plaintext)
			}
		})
	}
}

func TestEncrypt_HeaderStructure(t *testing.T) {
	clientPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}

	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatalf("generate auth secret: %v", err)
	}

	sub := webpush.Subscription{
		Endpoint: "https://push.example.com/sub/test-client",
		Keys: webpush.SubscriptionKeys{
			P256dh: base64.RawURLEncoding.EncodeToString(clientPriv.PublicKey().Bytes()),
			Auth:   base64.RawURLEncoding.EncodeToString(authSecret),
		},
	}

	plaintext := []byte("Header test")
	encrypted, err := webpush.Encrypt(plaintext, sub)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	// RFC 8188 header length for RFC 8291 is 86 bytes:
	// salt (16) + rs (4) + idlen (1) + serverPub (65) = 86 bytes
	// plus at least plaintext (11) + delimiter (1) + gcm tag (16) = 28 bytes
	if len(encrypted) < 86+len(plaintext)+1+16 {
		t.Fatalf("encrypted data too short: got %d bytes", len(encrypted))
	}

	// salt: 16 bytes
	salt := encrypted[0:16]
	allZero := true
	for _, b := range salt {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Errorf("salt is all zeros")
	}

	// rs: 4 bytes big-endian uint32, must be 4096
	rs := binary.BigEndian.Uint32(encrypted[16:20])
	if rs != 4096 {
		t.Errorf("rs = %d, want 4096", rs)
	}

	// idlen: 1 byte, must be 65
	idlen := encrypted[20]
	if idlen != 65 {
		t.Errorf("idlen = %d, want 65", idlen)
	}

	// serverPub: 65 bytes uncompressed EC point (starts with 0x04)
	serverPubBytes := encrypted[21:86]
	if serverPubBytes[0] != 0x04 {
		t.Errorf("serverPub[0] = 0x%02x, want 0x04 (uncompressed)", serverPubBytes[0])
	}
	serverPub, err := ecdh.P256().NewPublicKey(serverPubBytes)
	if err != nil {
		t.Errorf("serverPub is not a valid P-256 public key: %v", err)
	}
	if serverPub == nil {
		t.Errorf("serverPub is nil")
	}
}

func TestEncrypt_ErrorHandling(t *testing.T) {
	clientPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	validPub := base64.RawURLEncoding.EncodeToString(clientPriv.PublicKey().Bytes())
	validAuth := base64.RawURLEncoding.EncodeToString([]byte("1234567890123456"))

	tests := []struct {
		name      string
		sub       webpush.Subscription
		plaintext []byte
		wantErr   string
	}{
		{
			name: "missing p256dh",
			sub: webpush.Subscription{
				Endpoint: "https://push.example.com",
				Keys: webpush.SubscriptionKeys{
					P256dh: "",
					Auth:   validAuth,
				},
			},
			plaintext: []byte("test"),
			wantErr:   "p256dh",
		},
		{
			name: "bad base64 p256dh",
			sub: webpush.Subscription{
				Endpoint: "https://push.example.com",
				Keys: webpush.SubscriptionKeys{
					P256dh: "!!!not-base64!!!",
					Auth:   validAuth,
				},
			},
			plaintext: []byte("test"),
			wantErr:   "decode",
		},
		{
			name: "invalid p256dh key (not on curve / wrong length)",
			sub: webpush.Subscription{
				Endpoint: "https://push.example.com",
				Keys: webpush.SubscriptionKeys{
					P256dh: base64.RawURLEncoding.EncodeToString([]byte("too short")),
					Auth:   validAuth,
				},
			},
			plaintext: []byte("test"),
			wantErr:   "public key",
		},
		{
			name: "missing auth secret",
			sub: webpush.Subscription{
				Endpoint: "https://push.example.com",
				Keys: webpush.SubscriptionKeys{
					P256dh: validPub,
					Auth:   "",
				},
			},
			plaintext: []byte("test"),
			wantErr:   "auth",
		},
		{
			name: "bad base64 auth secret",
			sub: webpush.Subscription{
				Endpoint: "https://push.example.com",
				Keys: webpush.SubscriptionKeys{
					P256dh: validPub,
					Auth:   "###not-base64###",
				},
			},
			plaintext: []byte("test"),
			wantErr:   "decode",
		},
		{
			name: "short auth secret (< 16 bytes)",
			sub: webpush.Subscription{
				Endpoint: "https://push.example.com",
				Keys: webpush.SubscriptionKeys{
					P256dh: validPub,
					Auth:   base64.RawURLEncoding.EncodeToString([]byte("short")),
				},
			},
			plaintext: []byte("test"),
			wantErr:   "exactly 16 bytes",
		},
		{
			name: "oversized plaintext (> 3993 bytes)",
			sub: webpush.Subscription{
				Endpoint: "https://push.example.com",
				Keys: webpush.SubscriptionKeys{
					P256dh: validPub,
					Auth:   validAuth,
				},
			},
			plaintext: make([]byte, 3994),
			wantErr:   "exceeds maximum",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := webpush.Encrypt(tc.plaintext, tc.sub)
			if err == nil {
				t.Fatalf("Encrypt expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErr)) {
				t.Errorf("Encrypt error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestRFC8291_AppendixA_Vector(t *testing.T) {
	// Values taken directly from RFC 8291 Appendix A
	cleanB64 := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, " ", ""), "\n", "")
	}

	uaPrivB64 := "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	uaPubB64 := cleanB64(`BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-
      JvLexhqUzORcx aOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4`)
	authB64 := "BTBZMqHH6r4Tts7J_aSIgg"
	expectedPlaintext := "When I grow up, I want to be a watermelon"

	asPubB64 := cleanB64(`BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIg
      Dll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8`)
	saltB64 := "DGv6ra1nlYgDCS1FRnbzlw"
	ciphertextB64 := cleanB64(`8pfeW0KbunFT06SuDKoJH9Ql87S1QUrd
      irN6GcG7sFz1y1sqLgVi1VhjVkHsUoEs bI_0LpXMuGvnzQ`)

	uaPrivBytes, err := base64.RawURLEncoding.DecodeString(uaPrivB64)
	if err != nil {
		t.Fatalf("decode ua_private: %v", err)
	}
	clientPriv, err := ecdh.P256().NewPrivateKey(uaPrivBytes)
	if err != nil {
		t.Fatalf("parse ua_private: %v", err)
	}

	uaPubBytes, err := base64.RawURLEncoding.DecodeString(uaPubB64)
	if err != nil {
		t.Fatalf("decode ua_public: %v", err)
	}
	if !bytes.Equal(clientPriv.PublicKey().Bytes(), uaPubBytes) {
		t.Fatalf("derived client pub does not match expected ua_public")
	}

	authSecret, err := base64.RawURLEncoding.DecodeString(authB64)
	if err != nil {
		t.Fatalf("decode auth: %v", err)
	}

	saltBytes, err := base64.RawURLEncoding.DecodeString(saltB64)
	if err != nil {
		t.Fatalf("decode salt: %v", err)
	}
	asPubBytes, err := base64.RawURLEncoding.DecodeString(asPubB64)
	if err != nil {
		t.Fatalf("decode as_public: %v", err)
	}
	cipherBytes, err := base64.RawURLEncoding.DecodeString(ciphertextB64)
	if err != nil {
		t.Fatalf("decode ciphertext: %v", err)
	}

	// Construct full RFC 8188 payload: salt (16) || rs (4) || idlen (1) || serverPub (65) || ciphertext
	var fullMessage []byte
	fullMessage = append(fullMessage, saltBytes...)
	rsBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(rsBytes, 4096)
	fullMessage = append(fullMessage, rsBytes...)
	fullMessage = append(fullMessage, byte(len(asPubBytes)))
	fullMessage = append(fullMessage, asPubBytes...)
	fullMessage = append(fullMessage, cipherBytes...)

	decrypted, err := webpush.DecryptForTest(fullMessage, clientPriv, authSecret)
	if err != nil {
		t.Fatalf("DecryptForTest failed on RFC 8291 Appendix A vector: %v", err)
	}

	if string(decrypted) != expectedPlaintext {
		t.Errorf("decrypted = %q, want %q", string(decrypted), expectedPlaintext)
	}
}

func TestDecryptForTest_Errors(t *testing.T) {
	clientPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	authSecret := make([]byte, 16)
	rand.Read(authSecret)

	sub := webpush.Subscription{
		Endpoint: "https://push.example.com",
		Keys: webpush.SubscriptionKeys{
			P256dh: base64.RawURLEncoding.EncodeToString(clientPriv.PublicKey().Bytes()),
			Auth:   base64.RawURLEncoding.EncodeToString(authSecret),
		},
	}

	encrypted, err := webpush.Encrypt([]byte("Valid message"), sub)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	tests := []struct {
		name       string
		data       []byte
		priv       *ecdh.PrivateKey
		auth       []byte
		wantErrSub string
	}{
		{
			name:       "nil private key",
			data:       encrypted,
			priv:       nil,
			auth:       authSecret,
			wantErrSub: "private key",
		},
		{
			name:       "short auth secret",
			data:       encrypted,
			priv:       clientPriv,
			auth:       []byte("short"),
			wantErrSub: "exactly 16 bytes",
		},
		{
			name:       "message shorter than header",
			data:       encrypted[:50],
			priv:       clientPriv,
			auth:       authSecret,
			wantErrSub: "too short",
		},
		{
			name: "invalid idlen != 65",
			data: func() []byte {
				corrupt := make([]byte, len(encrypted))
				copy(corrupt, encrypted)
				corrupt[20] = 32 // change idlen
				return corrupt
			}(),
			priv:       clientPriv,
			auth:       authSecret,
			wantErrSub: "idlen",
		},
		{
			name:       "wrong auth secret",
			data:       encrypted,
			priv:       clientPriv,
			auth:       []byte("0123456789012345"),
			wantErrSub: "decrypt",
		},
		{
			name: "tampered ciphertext",
			data: func() []byte {
				corrupt := make([]byte, len(encrypted))
				copy(corrupt, encrypted)
				corrupt[len(corrupt)-1] ^= 0x01
				return corrupt
			}(),
			priv:       clientPriv,
			auth:       authSecret,
			wantErrSub: "decrypt",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := webpush.DecryptForTest(tc.data, tc.priv, tc.auth)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrSub)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErrSub)) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrSub)
			}
		})
	}
}
