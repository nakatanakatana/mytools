package webpush

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestGenerateVAPIDKeys(t *testing.T) {
	keys, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys failed: %v", err)
	}
	if len(keys.PrivateKey) == 0 || len(keys.PublicKey) == 0 {
		t.Fatal("expected non-empty private and public keys")
	}

	pubBytes, err := base64.RawURLEncoding.DecodeString(keys.PublicKey)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	if len(pubBytes) != 65 || pubBytes[0] != 0x04 {
		t.Fatalf("expected 65-byte uncompressed public key point, got len=%d first_byte=%x", len(pubBytes), pubBytes[0])
	}
}

func TestBuildVAPIDHeader(t *testing.T) {
	keys, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys failed: %v", err)
	}

	endpoint := "https://push.services.mozilla.com/wpush/v2/gAAAAAB..."
	headerVal, err := BuildVAPIDHeader(endpoint, "mailto:admin@example.com", keys, time.Now().Add(12*time.Hour))
	if err != nil {
		t.Fatalf("BuildVAPIDHeader failed: %v", err)
	}

	if !strings.HasPrefix(headerVal, "vapid ") {
		t.Fatalf("expected 'vapid ' prefix, got %q", headerVal)
	}
	parts := strings.Split(strings.TrimPrefix(headerVal, "vapid "), ", ")
	var token, kPub string
	for _, part := range parts {
		if strings.HasPrefix(part, "t=") {
			token = strings.TrimPrefix(part, "t=")
		}
		if strings.HasPrefix(part, "k=") {
			kPub = strings.TrimPrefix(part, "k=")
		}
	}
	if token == "" || kPub != keys.PublicKey {
		t.Fatalf("invalid vapid parts: token=%q, kPub=%q", token, kPub)
	}

	// Verify JWT structure: header.payload.signature
	jwtParts := strings.Split(token, ".")
	if len(jwtParts) != 3 {
		t.Fatalf("expected 3 parts in JWT, got %d", len(jwtParts))
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(jwtParts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var hdr map[string]string
	if err := json.Unmarshal(headerJSON, &hdr); err != nil || hdr["alg"] != "ES256" || hdr["typ"] != "JWT" {
		t.Fatalf("unexpected JWT header: %s", string(headerJSON))
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(jwtParts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims["aud"] != "https://push.services.mozilla.com" {
		t.Fatalf("expected aud https://push.services.mozilla.com, got %v", claims["aud"])
	}
	if claims["sub"] != "mailto:admin@example.com" {
		t.Fatalf("expected sub mailto:admin@example.com, got %v", claims["sub"])
	}

	// Verify signature using public key
	sigBytes, err := base64.RawURLEncoding.DecodeString(jwtParts[2])
	if err != nil || len(sigBytes) != 64 {
		t.Fatalf("invalid raw signature len=%d err=%v", len(sigBytes), err)
	}
	r := new(big.Int).SetBytes(sigBytes[:32])
	s := new(big.Int).SetBytes(sigBytes[32:])

	pubBytes, _ := base64.RawURLEncoding.DecodeString(keys.PublicKey)
	pubKey, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pubBytes)
	if err != nil {
		t.Fatalf("ParseUncompressedPublicKey failed: %v", err)
	}

	signedData := []byte(jwtParts[0] + "." + jwtParts[1])
	h := sha256.Sum256(signedData)
	if !ecdsa.Verify(pubKey, h[:], r, s) {
		t.Fatal("JWT signature verification failed")
	}
}

func TestParsePrivateKey_RawScalar(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), nil)
	if err != nil {
		t.Fatalf("GenerateKey failed: %v", err)
	}
	rawBytes, err := priv.Bytes()
	if err != nil {
		t.Fatalf("priv.Bytes failed: %v", err)
	}
	rawB64 := base64.RawURLEncoding.EncodeToString(rawBytes)

	parsed, err := ParsePrivateKey(rawB64)
	if err != nil {
		t.Fatalf("ParsePrivateKey failed for raw scalar: %v", err)
	}
	if !parsed.Equal(priv) {
		t.Fatal("parsed raw private key does not match original")
	}

	// Error on zero scalar
	zeroScalar := make([]byte, 32)
	zeroB64 := base64.RawURLEncoding.EncodeToString(zeroScalar)
	if _, err := ParsePrivateKey(zeroB64); err == nil {
		t.Fatal("expected error for zero scalar private key, got nil")
	}
}

func TestParsePrivateKey_PKCS8(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), nil)
	if err != nil {
		t.Fatalf("GenerateKey failed: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey failed: %v", err)
	}
	pkcs8B64 := base64.RawURLEncoding.EncodeToString(der)

	parsed, err := ParsePrivateKey(pkcs8B64)
	if err != nil {
		t.Fatalf("ParsePrivateKey failed for PKCS#8: %v", err)
	}
	if !parsed.Equal(priv) {
		t.Fatal("parsed private key does not match original")
	}
}

func TestParsePrivateKey_PKCS8_NonP256(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P384(), nil)
	if err != nil {
		t.Fatalf("GenerateKey failed: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey failed: %v", err)
	}
	pkcs8B64 := base64.RawURLEncoding.EncodeToString(der)

	if _, err := ParsePrivateKey(pkcs8B64); err == nil {
		t.Fatal("expected error for non-P256 PKCS#8 key, got nil")
	}
}

func TestParsePrivateKey_Errors(t *testing.T) {
	if _, err := ParsePrivateKey("???invalid-base64???"); err == nil {
		t.Fatal("expected error for invalid base64, got nil")
	}
	short := base64.RawURLEncoding.EncodeToString([]byte("too short"))
	if _, err := ParsePrivateKey(short); err == nil {
		t.Fatal("expected error for invalid key length, got nil")
	}
}

func TestBuildVAPIDHeader_Errors(t *testing.T) {
	if _, err := BuildVAPIDHeader("https://example.com", "mailto:test@example.com", nil, time.Now()); err == nil {
		t.Fatal("expected error for nil keys, got nil")
	}
	keys := &VAPIDKeys{
		PrivateKey: "invalid-key",
		PublicKey:  "invalid-pub",
	}
	if _, err := BuildVAPIDHeader("https://example.com", "mailto:test@example.com", keys, time.Now()); err == nil {
		t.Fatal("expected error for invalid private key, got nil")
	}
	keysValid, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys failed: %v", err)
	}
	keysEmptyPub := &VAPIDKeys{
		PrivateKey: keysValid.PrivateKey,
		PublicKey:  "",
	}
	if _, err := BuildVAPIDHeader("https://example.com", "mailto:test@example.com", keysEmptyPub, time.Now()); err == nil {
		t.Fatal("expected error for empty public key, got nil")
	}
	if _, err := BuildVAPIDHeader(":\ninvalid url", "mailto:test@example.com", keysValid, time.Now()); err == nil {
		t.Fatal("expected error for invalid endpoint url, got nil")
	}
	if _, err := BuildVAPIDHeader("/relative/path", "mailto:test@example.com", keysValid, time.Now()); err == nil {
		t.Fatal("expected error for endpoint without scheme/host, got nil")
	}
	if _, err := BuildVAPIDHeader("https://", "mailto:test@example.com", keysValid, time.Now()); err == nil {
		t.Fatal("expected error for endpoint without host, got nil")
	}
	if _, err := BuildVAPIDHeader("https://example.com", "invalid-subject", keysValid, time.Now()); err == nil {
		t.Fatal("expected error for subject without mailto: or https:, got nil")
	}
	if _, err := BuildVAPIDHeader("https://example.com", "", keysValid, time.Now()); err == nil {
		t.Fatal("expected error for empty subject, got nil")
	}
}

func TestValidateVAPIDKeys(t *testing.T) {
	validKeys, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys failed: %v", err)
	}

	if err := ValidateVAPIDKeys(*validKeys); err != nil {
		t.Fatalf("ValidateVAPIDKeys failed for valid keys: %v", err)
	}

	// Empty keys
	if err := ValidateVAPIDKeys(VAPIDKeys{}); err == nil {
		t.Fatal("expected error for empty VAPIDKeys, got nil")
	}

	// Mismatched keys
	otherKeys, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys failed: %v", err)
	}
	mismatched := VAPIDKeys{
		PrivateKey: validKeys.PrivateKey,
		PublicKey:  otherKeys.PublicKey,
	}
	if err := ValidateVAPIDKeys(mismatched); err == nil {
		t.Fatal("expected error for mismatched VAPIDKeys, got nil")
	}

	// Invalid private key
	invalidPriv := VAPIDKeys{
		PrivateKey: "invalid-base64",
		PublicKey:  validKeys.PublicKey,
	}
	if err := ValidateVAPIDKeys(invalidPriv); err == nil {
		t.Fatal("expected error for invalid private key, got nil")
	}
}

func TestBuildVAPIDHeader_AudNormalization(t *testing.T) {
	keys, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys failed: %v", err)
	}

	header, err := BuildVAPIDHeader("https://example.com:443/push/v1", "mailto:test@example.com", keys, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("BuildVAPIDHeader failed: %v", err)
	}

	// Token is in format "vapid t=<jwt>, k=<key>"
	parts := strings.Split(header, ", ")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "vapid t=") {
		t.Fatalf("unexpected header format: %s", header)
	}
	jwtToken := strings.TrimPrefix(parts[0], "vapid t=")
	jwtParts := strings.Split(jwtToken, ".")
	if len(jwtParts) != 3 {
		t.Fatalf("unexpected jwt token format: %s", jwtToken)
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(jwtParts[1])
	if err != nil {
		t.Fatalf("decode jwt payload failed: %v", err)
	}

	var claims map[string]any
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims failed: %v", err)
	}

	if claims["aud"] != "https://example.com" {
		t.Errorf("expected aud https://example.com, got %v", claims["aud"])
	}

	// Test IPv6 host preserves brackets
	header6, err := BuildVAPIDHeader("https://[2001:db8::1]:443/push/v1", "mailto:test@example.com", keys, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("BuildVAPIDHeader failed for IPv6: %v", err)
	}
	parts6 := strings.Split(header6, ", ")
	jwtToken6 := strings.TrimPrefix(parts6[0], "vapid t=")
	jwtParts6 := strings.Split(jwtToken6, ".")
	payloadJSON6, _ := base64.RawURLEncoding.DecodeString(jwtParts6[1])
	var claims6 map[string]any
	_ = json.Unmarshal(payloadJSON6, &claims6)
	if claims6["aud"] != "https://[2001:db8::1]" {
		t.Errorf("expected aud https://[2001:db8::1], got %v", claims6["aud"])
	}
}
