package webpush

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"encoding/binary"
	"errors"
	"fmt"
)

// DecryptForTest decrypts an encrypted Web Push payload using the client's private key and auth secret.
func DecryptForTest(data []byte, clientPriv *ecdh.PrivateKey, authSecret []byte) ([]byte, error) {
	if clientPriv == nil {
		return nil, errors.New("client private key is required")
	}
	if len(authSecret) != 16 {
		return nil, errors.New("auth secret must be exactly 16 bytes (RFC 8291 requires exactly 16 bytes)")
	}

	// Minimum header: salt (16) + rs (4) + idlen (1) = 21 bytes
	if len(data) < 21 {
		return nil, errors.New("data too short: header incomplete")
	}

	salt := data[0:16]
	rs := binary.BigEndian.Uint32(data[16:20])
	if rs < 18 {
		return nil, fmt.Errorf("invalid record size rs: got %d, want at least 18", rs)
	}
	idlen := int(data[20])
	if idlen != 65 {
		return nil, fmt.Errorf("invalid idlen: got %d, want 65", idlen)
	}

	headerEnd := 21 + idlen
	// Ciphertext must contain at least 1 byte delimiter + 16 bytes GCM tag
	if len(data) < headerEnd+17 {
		return nil, errors.New("data too short: missing ciphertext")
	}

	serverPubBytes := data[21:headerEnd]
	serverPub, err := ecdh.P256().NewPublicKey(serverPubBytes)
	if err != nil {
		return nil, fmt.Errorf("parse server public key: %w", err)
	}

	clientPubBytes := clientPriv.PublicKey().Bytes()
	ecdhSecret, err := clientPriv.ECDH(serverPub)
	if err != nil {
		return nil, fmt.Errorf("compute ECDH secret: %w", err)
	}

	cek, nonce, err := deriveKeys(ecdhSecret, authSecret, clientPubBytes, serverPubBytes, salt)
	if err != nil {
		return nil, err
	}

	ciphertext := data[headerEnd:]
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	record, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt ciphertext: %w", err)
	}

	if len(record) == 0 {
		return nil, errors.New("decrypted record is empty")
	}

	delimIdx := len(record) - 1
	for delimIdx >= 0 && record[delimIdx] == 0 {
		delimIdx--
	}
	if delimIdx < 0 || record[delimIdx] != 0x02 {
		return nil, errors.New("invalid record delimiter: expected 0x02")
	}

	plaintext := record[:delimIdx]
	return plaintext, nil
}
