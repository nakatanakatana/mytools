package relayclient

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func TestSignNIP98SignsExactRequest(t *testing.T) {
	key := nostr.Generate()
	now := time.Unix(1_800_000_000, 987_654_321)
	payload := []byte(`{"method":"allowpubkey","params":["abc"]}`)

	header, err := signNIP98("https://relay.example/manage?token=public", http.MethodPost, payload, key, now)
	if err != nil {
		t.Fatal(err)
	}
	event := decodeNIP98Header(t, header)
	if event.Kind != 27235 || event.CreatedAt != nostr.Timestamp(now.Unix()) {
		t.Fatalf("event kind/time = %d/%d", event.Kind, event.CreatedAt)
	}
	if event.PubKey != key.Public() || !event.CheckID() || !event.VerifySignature() {
		t.Fatal("event is not signed by the admin key")
	}
	wantHash := sha256.Sum256(payload)
	wantTags := map[string]string{"u": "https://relay.example/manage?token=public", "method": http.MethodPost, "payload": hex.EncodeToString(wantHash[:])}
	if len(event.Tags) != len(wantTags) {
		t.Fatalf("tags = %#v", event.Tags)
	}
	for _, tag := range event.Tags {
		if len(tag) != 2 || wantTags[tag[0]] != tag[1] {
			t.Fatalf("unexpected tag %#v", tag)
		}
		delete(wantTags, tag[0])
	}
	if len(wantTags) != 0 {
		t.Fatalf("missing tags: %#v", wantTags)
	}
}

func TestSignNIP98RejectsInvalidInputs(t *testing.T) {
	key := nostr.Generate()
	for _, test := range []struct {
		name, rawURL, method string
		key                  nostr.SecretKey
	}{
		{name: "relative URL", rawURL: "/manage", method: http.MethodPost, key: key},
		{name: "URL fragment", rawURL: "https://relay.example/manage#secret", method: http.MethodPost, key: key},
		{name: "non-HTTP URL", rawURL: "ftp://relay.example/manage", method: http.MethodPost, key: key},
		{name: "missing method", rawURL: "https://relay.example/manage", key: key},
		{name: "zero key", rawURL: "https://relay.example/manage", method: http.MethodPost},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := signNIP98(test.rawURL, test.method, nil, test.key, time.Now()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func decodeNIP98Header(t *testing.T, header string) nostr.Event {
	t.Helper()
	const prefix = "Nostr "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		t.Fatalf("Authorization = %q", header)
	}
	raw, err := base64.StdEncoding.DecodeString(header[len(prefix):])
	if err != nil {
		t.Fatal(err)
	}
	var event nostr.Event
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	return event
}
