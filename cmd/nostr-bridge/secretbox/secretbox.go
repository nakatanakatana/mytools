// Package secretbox encrypts provider credentials for persistence.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
)

// Box seals payloads with AES-256-GCM. The nonce is prefixed to each ciphertext.
type Box struct{ aead cipher.AEAD }

func New(key []byte) (Box, error) {
	if len(key) != 32 {
		return Box{}, errors.New("secretbox requires a 32-byte key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Box{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Box{}, err
	}
	return Box{aead: aead}, nil
}

func (b Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, nil), nil
}

func (b Box) Open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < b.aead.NonceSize() {
		return nil, errors.New("short encrypted payload")
	}
	return b.aead.Open(nil, ciphertext[:b.aead.NonceSize()], ciphertext[b.aead.NonceSize():], nil)
}
