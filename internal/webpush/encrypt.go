package webpush

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
)

const (
	// defaultRecordSize is the default record size for Web Push RFC 8188 (4096 bytes).
	defaultRecordSize = 4096

	// headerSize is the fixed size of RFC 8188 header for Web Push (86 bytes).
	// salt (16 bytes) + rs (4 bytes) + idlen (1 byte) + serverPub (65 bytes)
	headerSize = 86

	// maxPlaintextLength is the maximum plaintext length for a single Web Push record.
	// 4096 (rs) - 86 (header) - 1 (delimiter) - 16 (GCM tag) = 3993 bytes.
	maxPlaintextLength = 3993
)

// SubscriptionKeys holds the client's P-256 public key and authentication secret.
type SubscriptionKeys struct {
	P256dh string `json:"p256dh"`
	Auth   string `json:"auth"`
}

// Subscription represents a Web Push subscription.
type Subscription struct {
	Endpoint string           `json:"endpoint"`
	Keys     SubscriptionKeys `json:"keys"`
}

// Encrypt encrypts plaintext for a Web Push subscription according to RFC 8291 and RFC 8188.
func Encrypt(plaintext []byte, sub Subscription) ([]byte, error) {
	if len(plaintext) > maxPlaintextLength {
		return nil, fmt.Errorf("payload length %d exceeds maximum allowable size %d", len(plaintext), maxPlaintextLength)
	}
	if sub.Keys.P256dh == "" {
		return nil, errors.New("subscription p256dh key is required")
	}
	if sub.Keys.Auth == "" {
		return nil, errors.New("subscription auth secret is required")
	}

	clientPubBytes, err := DecodeBase64Flex(sub.Keys.P256dh)
	if err != nil {
		return nil, fmt.Errorf("decode p256dh key: %w", err)
	}

	clientPub, err := ecdh.P256().NewPublicKey(clientPubBytes)
	if err != nil {
		return nil, fmt.Errorf("parse p256dh public key: %w", err)
	}

	authSecret, err := DecodeBase64Flex(sub.Keys.Auth)
	if err != nil {
		return nil, fmt.Errorf("decode auth secret: %w", err)
	}
	if len(authSecret) != 16 {
		return nil, errors.New("auth secret must be exactly 16 bytes (RFC 8291 requires exactly 16 bytes)")
	}

	serverPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral server key: %w", err)
	}
	serverPubBytes := serverPriv.PublicKey().Bytes()

	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}

	ecdhSecret, err := serverPriv.ECDH(clientPub)
	if err != nil {
		return nil, fmt.Errorf("compute ECDH secret: %w", err)
	}

	cek, nonce, err := deriveKeys(ecdhSecret, authSecret, clientPubBytes, serverPubBytes, salt)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	// Single record: plaintext || delimiter 0x02
	record := append(slices.Clone(plaintext), 0x02)

	// RFC 8188 aes128gcm header: salt (16) || rs (4) || idlen (1) || keyid (65) || ciphertext
	result := make([]byte, 0, headerSize+len(record)+gcm.Overhead())
	result = append(result, salt...)
	result = binary.BigEndian.AppendUint32(result, defaultRecordSize)
	result = append(result, byte(len(serverPubBytes)))
	result = append(result, serverPubBytes...)
	result = gcm.Seal(result, nonce, record, nil)

	return result, nil
}


// deriveKeys derives CEK and Nonce using RFC 8291 Section 3.4 and RFC 8188 Section 2.2.
func deriveKeys(ecdhSecret, authSecret, clientPubBytes, serverPubBytes, salt []byte) (cek, nonce []byte, err error) {
	// RFC 8291: key_info = "WebPush: info" || 0x00 || ua_public || as_public
	keyInfo := slices.Concat([]byte("WebPush: info\x00"), clientPubBytes, serverPubBytes)

	ikm, err := hkdf.Key(sha256.New, ecdhSecret, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, nil, fmt.Errorf("derive IKM: %w", err)
	}

	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, fmt.Errorf("extract PRK: %w", err)
	}

	cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, nil, fmt.Errorf("derive CEK: %w", err)
	}

	nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, nil, fmt.Errorf("derive Nonce: %w", err)
	}

	return cek, nonce, nil
}
