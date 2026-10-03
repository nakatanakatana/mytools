package webpush

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// VAPIDKeys holds the base64url-encoded private and public keys.
type VAPIDKeys struct {
	PrivateKey string `json:"private_key"` // Base64URL-encoded raw 32-byte D or PKCS#8
	PublicKey  string `json:"public_key"`  // Base64URL-encoded uncompressed 65-byte point
}

// GenerateVAPIDKeys generates a new ECDSA P-256 key pair formatted for VAPID.
func GenerateVAPIDKeys() (*VAPIDKeys, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ECDSA P-256 key: %w", err)
	}

	privBytes, err := priv.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode private key: %w", err)
	}

	pubBytes, err := priv.PublicKey.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode public key: %w", err)
	}

	return &VAPIDKeys{
		PrivateKey: base64.RawURLEncoding.EncodeToString(privBytes),
		PublicKey:  base64.RawURLEncoding.EncodeToString(pubBytes),
	}, nil
}

// ParsePrivateKey parses a Base64/Base64URL private key (raw 32-byte scalar or PKCS#8).
func ParsePrivateKey(privStr string) (*ecdsa.PrivateKey, error) {
	data, err := DecodeBase64Flex(privStr)
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}

	// If PKCS#8 DER
	if key, err := x509.ParsePKCS8PrivateKey(data); err == nil {
		if ecKey, ok := key.(*ecdsa.PrivateKey); ok && ecKey.Curve == elliptic.P256() {
			return ecKey, nil
		}
		return nil, errors.New("PKCS#8 key is not an ECDSA P-256 private key")
	}

	// If raw 32-byte scalar
	if len(data) == 32 {
		priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), data)
		if err != nil {
			return nil, fmt.Errorf("parse raw private key scalar: %w", err)
		}
		return priv, nil
	}

	return nil, fmt.Errorf("unsupported private key format (len=%d)", len(data))
}

// ValidateVAPIDKeys validates that the private and public keys are valid ECDSA P-256 keys and match each other.
func ValidateVAPIDKeys(keys VAPIDKeys) error {
	if keys.PrivateKey == "" {
		return errors.New("vapid private key is required")
	}
	if keys.PublicKey == "" {
		return errors.New("vapid public key is required")
	}

	priv, err := ParsePrivateKey(keys.PrivateKey)
	if err != nil {
		return fmt.Errorf("parse vapid private key: %w", err)
	}

	derivedPubBytes, err := priv.PublicKey.Bytes()
	if err != nil {
		return fmt.Errorf("encode derived public key: %w", err)
	}

	configuredPubBytes, err := DecodeBase64Flex(keys.PublicKey)
	if err != nil {
		return fmt.Errorf("decode vapid public key: %w", err)
	}
	if subtle.ConstantTimeCompare(derivedPubBytes, configuredPubBytes) != 1 {
		return errors.New("vapid public key does not match private key")
	}

	return nil
}

// BuildVAPIDHeader formats the 'Authorization: vapid t=..., k=...' header.
func BuildVAPIDHeader(endpoint, subject string, keys *VAPIDKeys, exp time.Time) (string, error) {
	if keys == nil {
		return "", errors.New("vapid keys required")
	}
	if keys.PublicKey == "" {
		return "", errors.New("vapid public key required")
	}
	if subject == "" {
		return "", errors.New("vapid subject is required")
	}
	if !strings.HasPrefix(subject, "mailto:") && !strings.HasPrefix(subject, "https://") {
		return "", fmt.Errorf("vapid subject must start with 'mailto:' or 'https:': %q", subject)
	}
	priv, err := ParsePrivateKey(keys.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("parse vapid private key: %w", err)
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return "", &redactedError{message: "webpush: invalid endpoint URL", err: err}
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("webpush: endpoint URL must contain scheme and host")
	}
	host := u.Host
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		host = strings.TrimSuffix(host, ":"+u.Port())
	}
	aud := u.Scheme + "://" + host

	headerJSON := `{"typ":"JWT","alg":"ES256"}`
	hdrPart := base64.RawURLEncoding.EncodeToString([]byte(headerJSON))

	claimsMap := map[string]any{
		"aud": aud,
		"exp": exp.Unix(),
		"sub": subject,
	}
	claimsBytes, err := json.Marshal(claimsMap)
	if err != nil {
		return "", fmt.Errorf("marshal vapid claims: %w", err)
	}
	claimsPart := base64.RawURLEncoding.EncodeToString(claimsBytes)

	signingInput := hdrPart + "." + claimsPart
	hashed := sha256.Sum256([]byte(signingInput))

	r, s, err := ecdsa.Sign(rand.Reader, priv, hashed[:])
	if err != nil {
		return "", fmt.Errorf("sign vapid token: %w", err)
	}

	// P1363 raw signature: r (32 bytes) || s (32 bytes)
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	sigPart := base64.RawURLEncoding.EncodeToString(sig)
	token := signingInput + "." + sigPart

	publicKey, err := NormalizeVAPIDPublicKey(keys.PublicKey)
	if err != nil {
		return "", fmt.Errorf("normalize vapid public key: %w", err)
	}
	return fmt.Sprintf("vapid t=%s, k=%s", token, publicKey), nil
}

// NormalizeVAPIDPublicKey decodes a supported public key format and returns the
// unpadded Base64URL representation required in VAPID headers.
func NormalizeVAPIDPublicKey(publicKey string) (string, error) {
	publicKeyBytes, err := DecodeBase64Flex(publicKey)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(publicKeyBytes), nil
}

// DecodeBase64Flex decodes a base64 string trying RawURLEncoding, URLEncoding, RawStdEncoding, and StdEncoding.
func DecodeBase64Flex(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("empty base64 string")
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.StdEncoding.DecodeString(s)
}
