package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	relaystore "github.com/nakatanakatana/mytools/cmd/nostr-relay/store"
)

func TestManagementSupportedMethodsRequiresAdminNIP98(t *testing.T) {
	handler, _, adminSK, now := managementTestHandler(t)
	body := []byte(`{"method":"supportedmethods","params":[]}`)

	unauthorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://internal:8080/relay", bytes.NewReader(body))
	request.Header.Set("Content-Type", managementMediaType)
	request.Header.Set("Forwarded", "host=attacker.example;proto=https")
	handler.ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code)
	}

	response := managementCall(t, handler, adminSK, now, "https://relay.example/relay", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var got struct {
		Result []string `json:"result"`
		Error  string   `json:"error"`
	}
	decodeManagementResponse(t, response, &got)
	want := []string{"allowpubkey", "unallowpubkey", "listallowedpubkeys"}
	if !reflect.DeepEqual(got.Result, want) || got.Error != "" {
		t.Fatalf("response = %#v, want result %v", got, want)
	}
}

func TestManagementAllowListAndUnallowAreIdempotent(t *testing.T) {
	handler, store, adminSK, now := managementTestHandler(t)
	first := nostr.GetPublicKey(nostr.Generate())
	second := nostr.GetPublicKey(nostr.Generate())
	if first.Hex() > second.Hex() {
		first, second = second, first
	}

	for _, call := range []struct {
		method string
		params any
	}{
		{"allowpubkey", []any{second.Hex(), "second reason"}},
		{"allowpubkey", []any{first.Hex(), "old reason"}},
		{"allowpubkey", []any{first.Hex()}},
	} {
		body, _ := json.Marshal(managementRequest{Method: call.method, Params: mustRawJSON(t, call.params)})
		response := managementCall(t, handler, adminSK, now, "https://relay.example/relay", body)
		var got struct {
			Result bool   `json:"result"`
			Error  string `json:"error"`
		}
		decodeManagementResponse(t, response, &got)
		if !got.Result || got.Error != "" {
			t.Fatalf("%s response = %#v", call.method, got)
		}
	}

	listBody := []byte(`{"method":"listallowedpubkeys","params":[]}`)
	response := managementCall(t, handler, adminSK, now, "https://relay.example/relay", listBody)
	var listed struct {
		Result []struct {
			PubKey string `json:"pubkey"`
			Reason string `json:"reason"`
		} `json:"result"`
		Error string `json:"error"`
	}
	decodeManagementResponse(t, response, &listed)
	if listed.Error != "" || len(listed.Result) != 2 {
		t.Fatalf("list response = %#v", listed)
	}
	if listed.Result[0].PubKey != first.Hex() || listed.Result[0].Reason != "" || listed.Result[1].PubKey != second.Hex() || listed.Result[1].Reason != "second reason" {
		t.Fatalf("sorted list with reasons = %#v", listed.Result)
	}

	unallow, _ := json.Marshal(managementRequest{Method: "unallowpubkey", Params: mustRawJSON(t, []any{first.Hex()})})
	for range 2 {
		response := managementCall(t, handler, adminSK, now, "https://relay.example/relay", unallow)
		var got struct {
			Result bool `json:"result"`
		}
		decodeManagementResponse(t, response, &got)
		if !got.Result {
			t.Fatalf("unallow result = false, body = %s", response.Body.String())
		}
	}
	allowed, err := store.PublisherAllowed(context.Background(), first)
	if err != nil || allowed {
		t.Fatalf("PublisherAllowed() = %v, %v, want false, nil", allowed, err)
	}
}

func TestManagementRejectsInvalidRequests(t *testing.T) {
	handler, _, adminSK, now := managementTestHandler(t)
	invalidPubkey := strings.Repeat("z", 64)
	tests := []struct {
		name string
		body []byte
	}{
		{"malformed JSON", []byte(`{"method":`)},
		{"missing method", []byte(`{"params":[]}`)},
		{"unknown method", []byte(`{"method":"banpubkey","params":[]}`)},
		{"supportedmethods params", []byte(`{"method":"supportedmethods","params":[1]}`)},
		{"list params", []byte(`{"method":"listallowedpubkeys","params":[1]}`)},
		{"allow arity", []byte(`{"method":"allowpubkey","params":[]}`)},
		{"allow too many params", []byte(`{"method":"allowpubkey","params":["` + strings.Repeat("0", 64) + `","",""]}`)},
		{"allow invalid pubkey", []byte(`{"method":"allowpubkey","params":["` + invalidPubkey + `",""]}`)},
		{"allow wrong reason type", []byte(`{"method":"allowpubkey","params":["` + strings.Repeat("0", 64) + `",1]}`)},
		{"unallow arity", []byte(`{"method":"unallowpubkey","params":[]}`)},
		{"unallow too many params", []byte(`{"method":"unallowpubkey","params":["` + strings.Repeat("0", 64) + `","",""]}`)},
		{"unallow wrong reason type", []byte(`{"method":"unallowpubkey","params":["` + strings.Repeat("0", 64) + `",1]}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := managementCall(t, handler, adminSK, now, "https://relay.example/relay", test.body)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			var got managementResponse
			decodeManagementResponse(t, response, &got)
			if got.Error == "" {
				t.Fatalf("error empty, body = %s", response.Body.String())
			}
		})
	}
}

func TestManagementRoutingAndBodyLimit(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next", "yes")
		w.WriteHeader(http.StatusTeapot)
	})
	store, err := relaystore.Open(t.TempDir() + "/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	adminSK := nostr.Generate()
	validator := NIP98Validator{AdminPubKey: nostr.GetPublicKey(adminSK), ExpectedURL: "https://relay.example/relay"}
	handler := NewManagementHandler(next, store, validator)

	for _, test := range []struct {
		name        string
		method      string
		contentType string
		duplicate   bool
	}{
		{"GET management media", http.MethodGet, managementMediaType, false},
		{"POST ordinary JSON", http.MethodPost, "application/json", false},
		{"POST media type parameter", http.MethodPost, managementMediaType + "; charset=utf-8", false},
		{name: "POST duplicate media type", method: http.MethodPost, contentType: managementMediaType, duplicate: true},
		{"websocket upgrade", http.MethodGet, "", false},
		{"NIP-11 GET", http.MethodGet, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, "http://internal:8080/relay", strings.NewReader(`{}`))
			r.Header.Set("Content-Type", test.contentType)
			if test.duplicate {
				r.Header.Add("Content-Type", managementMediaType)
			}
			if test.name == "websocket upgrade" {
				r.Header.Set("Upgrade", "websocket")
			}
			if test.name == "NIP-11 GET" {
				r.Header.Set("Accept", "application/nostr+json")
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusTeapot || w.Header().Get("X-Next") != "yes" {
				t.Fatalf("request did not pass through: status=%d headers=%v", w.Code, w.Header())
			}
		})
	}

	large := []byte(`{"method":"supportedmethods","params":[],"padding":"` + strings.Repeat("x", 70<<10) + `"}`)
	response := managementCall(t, handler, adminSK, time.Now(), "https://relay.example/relay", large)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body status = %d, want 413", response.Code)
	}
}

func TestManagementPrivateRelayWiringAndPublicMode(t *testing.T) {
	adminSK := nostr.Generate()
	reader := nostr.GetPublicKey(nostr.Generate())
	private, err := NewRelay(context.Background(), Config{
		Mode: ModePrivateSQLite, DatabasePath: t.TempDir() + "/relay.db", ServiceURL: "https://relay.example/relay",
		AdminPubkey: nostr.GetPublicKey(adminSK).Hex(), ReaderPubkeys: []string{reader.Hex()}, MaxQueryLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = private.Close() }()
	body := []byte(`{"method":"supportedmethods","params":[]}`)
	response := managementCall(t, private.Handler, adminSK, time.Now(), "https://relay.example/relay", body)
	if response.Code != http.StatusOK {
		t.Fatalf("private management status = %d, body = %s", response.Code, response.Body.String())
	}
	protocolResponse := managementCall(t, private.ProtocolHandler, adminSK, time.Now(), "https://relay.example/relay", body)
	if protocolResponse.Code == http.StatusOK && strings.Contains(protocolResponse.Body.String(), `"result":true`) {
		t.Fatalf("protocol listener exposed management response: %s", protocolResponse.Body.String())
	}
	if private.ManagementHandler == nil {
		t.Fatal("private relay has no dedicated management handler")
	}

	public, err := NewRelay(context.Background(), Config{Mode: ModePublicMemory, MaxQueryLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = public.Close() }()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "http://relay.example/", bytes.NewReader(body))
	r.Header.Set("Content-Type", managementMediaType)
	public.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"error":"missing auth"`) {
		t.Fatalf("public legacy POST response changed: status=%d body=%s", w.Code, w.Body.String())
	}
}

func managementTestHandler(t *testing.T) (http.Handler, *relaystore.SQLiteStore, nostr.SecretKey, time.Time) {
	t.Helper()
	store, err := relaystore.Open(t.TempDir() + "/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	adminSK := nostr.Generate()
	now := time.Unix(1_800_000_000, 0)
	validator := NIP98Validator{AdminPubKey: nostr.GetPublicKey(adminSK), Now: func() time.Time { return now }, ExpectedURL: "https://relay.example/relay"}
	return NewManagementHandler(http.NotFoundHandler(), store, validator), store, adminSK, now
}

func managementCall(t *testing.T, handler http.Handler, sk nostr.SecretKey, now time.Time, canonicalURL string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://relay-internal:8080/relay", bytes.NewReader(body))
	r.Host = "relay-internal:8080"
	r.Header.Set("Content-Type", managementMediaType)
	hash := sha256.Sum256(body)
	event := nostr.Event{CreatedAt: nostr.Timestamp(now.Unix()), Kind: nip98Kind, Tags: nostr.Tags{
		{"u", canonicalURL}, {"method", http.MethodPost}, {"payload", hex.EncodeToString(hash[:])},
	}}
	if err := event.Sign(sk); err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Nostr "+base64.StdEncoding.EncodeToString([]byte(event.String())))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func decodeManagementResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if got := response.Header().Get("Content-Type"); got != managementMediaType {
		t.Fatalf("Content-Type = %q, want %q", got, managementMediaType)
	}
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
