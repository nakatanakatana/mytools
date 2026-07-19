package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func TestNIP98Validate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	adminSK := nostr.Generate()
	admin := nostr.GetPublicKey(adminSK)
	otherSK := nostr.Generate()
	payload := []byte(`{"method":"supportedmethods"}`)
	requestURL := "https://relay.example/manage?scope=all"

	newEvent := func(sk nostr.SecretKey) nostr.Event {
		hash := sha256.Sum256(payload)
		event := nostr.Event{
			CreatedAt: nostr.Timestamp(now.Unix()),
			Kind:      27235,
			Tags: nostr.Tags{
				{"u", requestURL},
				{"method", http.MethodPost},
				{"payload", hex.EncodeToString(hash[:])},
			},
		}
		if err := event.Sign(sk); err != nil {
			t.Fatal(err)
		}
		return event
	}

	authorize := func(t *testing.T, event nostr.Event) *http.Request {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, requestURL, strings.NewReader(string(payload)))
		r.Header.Set("Authorization", "Nostr "+base64.StdEncoding.EncodeToString([]byte(event.String())))
		return r
	}

	validator := NIP98Validator{AdminPubKey: admin, Now: func() time.Time { return now }, MaxSkew: 60 * time.Second}

	t.Run("accepts a signed admin event bound to the request", func(t *testing.T) {
		if err := validator.Validate(authorize(t, newEvent(adminSK)), payload); err != nil {
			t.Fatalf("Validate() error = %v", err)
		}
	})

	for _, offset := range []time.Duration{-60 * time.Second, 60 * time.Second} {
		t.Run("accepts exact clock skew boundary "+offset.String(), func(t *testing.T) {
			event := newEvent(adminSK)
			event.CreatedAt = nostr.Timestamp(now.Add(offset).Unix())
			event = resign(t, event, adminSK)
			if err := validator.Validate(authorize(t, event), payload); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}

	t.Run("rejects a mutated supplied event ID", func(t *testing.T) {
		event := newEvent(adminSK)
		event.ID[0] ^= 0xff
		if err := validator.Validate(authorize(t, event), payload); err == nil {
			t.Fatal("Validate() error = nil, want rejection")
		}
	})

	validAuthorization := func(event nostr.Event) string {
		return "Nostr " + base64.StdEncoding.EncodeToString([]byte(event.String()))
	}
	for _, duplicate := range []struct {
		name    string
		headers [2]string
	}{
		{name: "valid then invalid", headers: [2]string{validAuthorization(newEvent(adminSK)), "Bearer invalid"}},
		{name: "invalid then valid", headers: [2]string{"Bearer invalid", validAuthorization(newEvent(adminSK))}},
	} {
		t.Run("rejects duplicate raw authorization headers "+duplicate.name, func(t *testing.T) {
			r := authorize(t, newEvent(adminSK))
			r.Header.Del("Authorization")
			r.Header.Add("Authorization", duplicate.headers[0])
			r.Header.Add("Authorization", duplicate.headers[1])
			if err := validator.Validate(r, payload); err == nil {
				t.Fatal("Validate() error = nil, want rejection")
			}
		})
	}

	t.Run("rejects an actual non-POST request with a signed POST tag", func(t *testing.T) {
		r := authorize(t, newEvent(adminSK))
		r.Method = http.MethodGet
		if err := validator.Validate(r, payload); err == nil {
			t.Fatal("Validate() error = nil, want rejection")
		}
	})

	tests := []struct {
		name   string
		header func(t *testing.T) string
		event  func(nostr.Event) nostr.Event
	}{
		{name: "missing authorization", header: func(*testing.T) string { return "" }},
		{name: "wrong authorization scheme", header: func(*testing.T) string { return "Bearer abc" }},
		{name: "lowercase authorization scheme", header: func(*testing.T) string { return "nostr abc" }},
		{name: "malformed base64", header: func(*testing.T) string { return "Nostr !!!" }},
		{name: "malformed event JSON", header: func(*testing.T) string { return "Nostr " + base64.StdEncoding.EncodeToString([]byte("{} garbage")) }},
		{name: "wrong kind", event: func(e nostr.Event) nostr.Event { e.Kind = 1; return resign(t, e, adminSK) }},
		{name: "invalid signature", event: func(e nostr.Event) nostr.Event { e.Sig[0] ^= 0xff; return e }},
		{name: "non-admin pubkey", event: func(e nostr.Event) nostr.Event { return resign(t, e, otherSK) }},
		{name: "wrong URL", event: func(e nostr.Event) nostr.Event {
			e.Tags[0][1] = "https://relay.example/other?scope=all"
			return resign(t, e, adminSK)
		}},
		{name: "URL query mismatch", event: func(e nostr.Event) nostr.Event {
			e.Tags[0][1] = "https://relay.example/manage?scope=other"
			return resign(t, e, adminSK)
		}},
		{name: "URL trailing slash mismatch", event: func(e nostr.Event) nostr.Event {
			e.Tags[0][1] = "https://relay.example/manage/?scope=all"
			return resign(t, e, adminSK)
		}},
		{name: "non-absolute URL", event: func(e nostr.Event) nostr.Event { e.Tags[0][1] = "/manage?scope=all"; return resign(t, e, adminSK) }},
		{name: "wrong method", event: func(e nostr.Event) nostr.Event { e.Tags[1][1] = http.MethodGet; return resign(t, e, adminSK) }},
		{name: "wrong payload hash", event: func(e nostr.Event) nostr.Event { e.Tags[2][1] = strings.Repeat("0", 64); return resign(t, e, adminSK) }},
		{name: "malformed payload hash", event: func(e nostr.Event) nostr.Event { e.Tags[2][1] = "not-hex"; return resign(t, e, adminSK) }},
		{name: "more than 60 seconds old", event: func(e nostr.Event) nostr.Event { e.CreatedAt -= 61; return resign(t, e, adminSK) }},
		{name: "more than 60 seconds in future", event: func(e nostr.Event) nostr.Event { e.CreatedAt += 61; return resign(t, e, adminSK) }},
		{name: "missing u tag", event: func(e nostr.Event) nostr.Event { e.Tags = e.Tags[1:]; return resign(t, e, adminSK) }},
		{name: "duplicate u tag", event: func(e nostr.Event) nostr.Event {
			e.Tags = append(e.Tags, nostr.Tag{"u", requestURL})
			return resign(t, e, adminSK)
		}},
		{name: "malformed u tag", event: func(e nostr.Event) nostr.Event {
			e.Tags[0] = nostr.Tag{"u", requestURL, "extra"}
			return resign(t, e, adminSK)
		}},
		{name: "missing method tag", event: func(e nostr.Event) nostr.Event {
			e.Tags = append(e.Tags[:1], e.Tags[2:]...)
			return resign(t, e, adminSK)
		}},
		{name: "duplicate method tag", event: func(e nostr.Event) nostr.Event {
			e.Tags = append(e.Tags, nostr.Tag{"method", http.MethodPost})
			return resign(t, e, adminSK)
		}},
		{name: "malformed method tag", event: func(e nostr.Event) nostr.Event { e.Tags[1] = nostr.Tag{"method"}; return resign(t, e, adminSK) }},
		{name: "missing payload tag", event: func(e nostr.Event) nostr.Event { e.Tags = e.Tags[:2]; return resign(t, e, adminSK) }},
		{name: "duplicate payload tag", event: func(e nostr.Event) nostr.Event { e.Tags = append(e.Tags, e.Tags[2]); return resign(t, e, adminSK) }},
		{name: "malformed payload tag", event: func(e nostr.Event) nostr.Event {
			e.Tags[2] = nostr.Tag{"payload", e.Tags[2][1], "extra"}
			return resign(t, e, adminSK)
		}},
	}

	for _, test := range tests {
		t.Run("rejects "+test.name, func(t *testing.T) {
			event := newEvent(adminSK)
			r := authorize(t, event)
			if test.event != nil {
				r = authorize(t, test.event(event))
			}
			if test.header != nil {
				r.Header.Set("Authorization", test.header(t))
			}
			if err := validator.Validate(r, payload); err == nil {
				t.Fatal("Validate() error = nil, want rejection")
			}
		})
	}
}

func TestNIP98ValidateUsesConfiguredCanonicalURLAndRequestTarget(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	sk := nostr.Generate()
	payload := []byte(`{"method":"supportedmethods","params":[]}`)
	hash := sha256.Sum256(payload)
	event := nostr.Event{CreatedAt: nostr.Timestamp(now.Unix()), Kind: nip98Kind, Tags: nostr.Tags{
		{"u", "https://relay.example/relay?scope=admin"},
		{"method", http.MethodPost},
		{"payload", hex.EncodeToString(hash[:])},
	}}
	if err := event.Sign(sk); err != nil {
		t.Fatal(err)
	}
	authorization := "Nostr " + base64.StdEncoding.EncodeToString([]byte(event.String()))
	validator := NIP98Validator{AdminPubKey: nostr.GetPublicKey(sk), Now: func() time.Time { return now }, ExpectedURL: "https://relay.example/relay?scope=admin"}
	request := func(target string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
		r.Header.Set("Forwarded", "host=attacker.example;proto=https")
		r.Header.Set("X-Forwarded-Host", "attacker.example")
		r.Header.Set("Authorization", authorization)
		return r
	}

	t.Run("accepts internal scheme and host with exact configured path and query", func(t *testing.T) {
		if err := validator.Validate(request("http://internal:8080/relay?scope=admin"), payload); err != nil {
			t.Fatalf("Validate() error = %v", err)
		}
	})
	t.Run("rejects a different incoming path", func(t *testing.T) {
		if err := validator.Validate(request("http://internal:8080/other?scope=admin"), payload); err == nil {
			t.Fatal("Validate() error = nil, want rejection")
		}
	})
	t.Run("rejects a different incoming query", func(t *testing.T) {
		if err := validator.Validate(request("http://internal:8080/relay?scope=other"), payload); err == nil {
			t.Fatal("Validate() error = nil, want rejection")
		}
	})
}

func resign(t *testing.T, event nostr.Event, sk nostr.SecretKey) nostr.Event {
	t.Helper()
	if err := event.Sign(sk); err != nil {
		t.Fatal(err)
	}
	return event
}
