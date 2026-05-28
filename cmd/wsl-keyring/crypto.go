package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"math/big"

	"golang.org/x/crypto/hkdf"
)

// IETF 1024-bit DH group (RFC 2409 / "Second Oakley Group")
// This is the group used by the Secret Service API specification.
var (
	dhPrime, _ = new(big.Int).SetString(
		"FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD1"+
			"29024E088A67CC74020BBEA63B139B22514A08798E3404DD"+
			"EF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245"+
			"E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7ED"+
			"EE386BFB5A899FA5AE9F24117C4B1FE649286651ECE65381"+
			"FFFFFFFFFFFFFFFF",
		16,
	)
	dhGenerator = big.NewInt(2)
)

// DHKeypair holds a DH private/public key pair.
type DHKeypair struct {
	Private *big.Int
	Public  *big.Int
}

// GenerateDHKeypair generates a new DH keypair using the IETF 1024-bit group.
func GenerateDHKeypair() (*DHKeypair, error) {
	// Generate a random private key in [2, p-2]
	max := new(big.Int).Sub(dhPrime, big.NewInt(2))
	private, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, err
	}
	private.Add(private, big.NewInt(2))

	// public = g^private mod p
	public := new(big.Int).Exp(dhGenerator, private, dhPrime)

	return &DHKeypair{
		Private: private,
		Public:  public,
	}, nil
}

// BigIntToBytes converts a big.Int to a 128-byte (1024-bit) big-endian byte slice.
func BigIntToBytes(n *big.Int) []byte {
	b := n.Bytes()
	if len(b) >= 128 {
		return b[len(b)-128:]
	}
	// Left-pad with zeros to 128 bytes
	padded := make([]byte, 128)
	copy(padded[128-len(b):], b)
	return padded
}

// ComputeSharedSecret computes the DH shared secret from the client's public key
// and the server's private key.
func ComputeSharedSecret(clientPublicBytes []byte, serverPrivate *big.Int) []byte {
	clientPublic := new(big.Int).SetBytes(clientPublicBytes)
	// shared = clientPublic^serverPrivate mod p
	shared := new(big.Int).Exp(clientPublic, serverPrivate, dhPrime)
	return BigIntToBytes(shared)
}

// DeriveAESKey derives a 16-byte AES-128 key from the DH shared secret using HKDF-SHA256.
// The Secret Service spec uses an empty salt and empty info.
func DeriveAESKey(sharedSecret []byte) ([]byte, error) {
	r := hkdf.New(sha256.New, sharedSecret, []byte{}, []byte{})
	key := make([]byte, 16)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, err
	}
	return key, nil
}

// PKCS7Pad pads plaintext to a multiple of blockSize using PKCS#7 padding.
func PKCS7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	padded := make([]byte, len(data)+padding)
	copy(padded, data)
	for i := len(data); i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	return padded
}

// PKCS7Unpad removes PKCS#7 padding from data.
func PKCS7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty data")
	}
	padding := int(data[len(data)-1])
	if padding == 0 || padding > aes.BlockSize {
		return nil, errors.New("invalid padding size")
	}
	if len(data) < padding {
		return nil, errors.New("data shorter than padding")
	}
	// Constant-time padding verification
	check := 0
	for _, b := range data[len(data)-padding:] {
		check |= int(subtle.ConstantTimeByteEq(b, byte(padding)) - 1)
	}
	if check != 0 {
		return nil, errors.New("invalid PKCS7 padding")
	}
	return data[:len(data)-padding], nil
}

// AESCBCEncrypt encrypts plaintext using AES-128-CBC with the given key.
// Returns (iv, ciphertext, error). A random IV is generated.
func AESCBCEncrypt(key, plaintext []byte) (iv []byte, ciphertext []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}

	padded := PKCS7Pad(plaintext, aes.BlockSize)
	ciphertext = make([]byte, len(padded))

	iv = make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, nil, err
	}

	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(ciphertext, padded)

	return iv, ciphertext, nil
}

// AESCBCDecrypt decrypts ciphertext using AES-128-CBC with the given key and IV.
func AESCBCDecrypt(key, iv, ciphertext []byte) ([]byte, error) {
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("ciphertext is not a multiple of block size")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	plaintext := make([]byte, len(ciphertext))
	mode := cipher.NewCBCDecrypter(block, iv)
	mode.CryptBlocks(plaintext, ciphertext)

	return PKCS7Unpad(plaintext)
}
