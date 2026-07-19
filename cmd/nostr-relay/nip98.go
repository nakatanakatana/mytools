package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"fiatjaf.com/nostr"
)

const nip98Kind nostr.Kind = 27235

type NIP98Validator struct {
	AdminPubKey nostr.PubKey
	ExpectedURL string
	Now         func() time.Time
	MaxSkew     time.Duration
	ReplayStore interface {
		ConsumeNIP98Event(context.Context, nostr.ID, time.Time, time.Duration) error
	}
}

func (v NIP98Validator) Validate(r *http.Request, payload []byte) error {
	authorizations := r.Header.Values("Authorization")
	if len(authorizations) != 1 {
		return errors.New("invalid NIP-98 authorization")
	}
	encoded, ok := strings.CutPrefix(authorizations[0], "Nostr ")
	if !ok || encoded == "" || strings.ContainsAny(encoded, " \t\r\n") {
		return errors.New("invalid NIP-98 authorization")
	}

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return errors.New("invalid NIP-98 authorization encoding")
	}

	var event nostr.Event
	if err := json.Unmarshal(raw, &event); err != nil {
		return errors.New("invalid NIP-98 authorization event")
	}
	if event.Kind != nip98Kind {
		return errors.New("invalid NIP-98 event kind")
	}
	if !event.CheckID() {
		return errors.New("invalid NIP-98 event ID")
	}
	if !event.VerifySignature() {
		return errors.New("invalid NIP-98 event signature")
	}
	if event.PubKey != v.AdminPubKey {
		return errors.New("NIP-98 signer is not the administrator")
	}

	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	maxSkew := v.MaxSkew
	if maxSkew <= 0 {
		maxSkew = 60 * time.Second
	}
	validationTime := now()
	createdAt := time.Unix(int64(event.CreatedAt), 0)
	if skew := validationTime.Sub(createdAt); skew > maxSkew || skew < -maxSkew {
		return errors.New("NIP-98 event timestamp is outside the allowed window")
	}

	u, err := singleNIP98Tag(event.Tags, "u")
	if err != nil {
		return err
	}
	eventURL, err := absoluteNIP98URL(u)
	if err != nil {
		return errors.New("invalid NIP-98 URL tag")
	}
	requestURL := v.ExpectedURL
	if requestURL == "" {
		requestURL, err = absoluteNIP98RequestURL(r)
	} else {
		requestURL, err = absoluteNIP98URL(requestURL)
		if err == nil && !nip98RequestTargetMatches(r, requestURL) {
			return errors.New("NIP-98 request target does not match configured URL")
		}
	}
	if err != nil || eventURL != requestURL {
		return errors.New("NIP-98 URL does not match request")
	}

	method, err := singleNIP98Tag(event.Tags, "method")
	if err != nil || method != http.MethodPost || method != r.Method {
		return errors.New("invalid NIP-98 method tag")
	}

	payloadTag, err := singleNIP98Tag(event.Tags, "payload")
	if err != nil {
		return err
	}
	wantHash := sha256.Sum256(payload)
	gotHash, err := hex.DecodeString(payloadTag)
	if err != nil || len(gotHash) != sha256.Size || subtle.ConstantTimeCompare(gotHash, wantHash[:]) != 1 {
		return errors.New("NIP-98 payload does not match request")
	}
	if v.ReplayStore != nil {
		// A timestamp at the future edge can remain valid until the past edge,
		// so retain IDs across the complete two-sided acceptance interval.
		if err := v.ReplayStore.ConsumeNIP98Event(r.Context(), event.ID, validationTime, 2*maxSkew); err != nil {
			return errors.New("NIP-98 authorization event was already used")
		}
	}

	return nil
}

func nip98RequestTargetMatches(r *http.Request, expectedURL string) bool {
	expected, err := url.Parse(expectedURL)
	if err != nil {
		return false
	}
	expectedPath := expected.EscapedPath()
	if expectedPath == "" {
		expectedPath = "/"
	}
	requestPath := r.URL.EscapedPath()
	if requestPath == "" {
		requestPath = "/"
	}
	return requestPath == expectedPath && r.URL.RawQuery == expected.RawQuery
}

func singleNIP98Tag(tags nostr.Tags, name string) (string, error) {
	var value string
	count := 0
	for _, tag := range tags {
		if len(tag) == 0 || tag[0] != name {
			continue
		}
		count++
		if len(tag) != 2 || tag[1] == "" {
			return "", errors.New("malformed NIP-98 tag")
		}
		value = tag[1]
	}
	if count != 1 {
		return "", errors.New("missing or duplicate NIP-98 tag")
	}
	return value, nil
}

func absoluteNIP98URL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("not an absolute HTTP URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("not an HTTP URL")
	}
	return u.String(), nil
}

func absoluteNIP98RequestURL(r *http.Request) (string, error) {
	if r.URL.IsAbs() {
		return absoluteNIP98URL(r.URL.String())
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return absoluteNIP98URL(scheme + "://" + r.Host + r.URL.RequestURI())
}
