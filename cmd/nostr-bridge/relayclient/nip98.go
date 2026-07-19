package relayclient

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"time"

	"fiatjaf.com/nostr"
)

const nip98Kind nostr.Kind = 27235

var errInvalidAdminKey = errors.New("invalid relay administrator key")

func signNIP98(rawURL, method string, payload []byte, key nostr.SecretKey, now time.Time) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("invalid relay management endpoint")
	}
	if strings.TrimSpace(method) == "" {
		return "", errors.New("invalid relay management method")
	}
	if key == (nostr.SecretKey{}) {
		return "", errInvalidAdminKey
	}
	event := nostr.Event{
		CreatedAt: nostr.Timestamp(now.Unix()),
		Kind:      nip98Kind,
		Tags: nostr.Tags{
			{"u", u.String()},
			{"method", method},
			{"payload", payloadHash(payload)},
		},
	}
	if err := event.Sign(key); err != nil {
		return "", errors.New("sign relay management request")
	}
	return "Nostr " + base64.StdEncoding.EncodeToString([]byte(event.String())), nil
}

func payloadHash(payload []byte) string {
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}
