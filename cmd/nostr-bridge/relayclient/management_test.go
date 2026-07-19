package relayclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func TestManagementAllowPubKeySendsSignedRequest(t *testing.T) {
	admin := nostr.Generate()
	pubkey := nostr.Generate().Public()
	now := time.Unix(1_800_000_000, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q", r.Method)
		}
		if got := r.Header.Values("Content-Type"); len(got) != 1 || got[0] != managementMediaType {
			t.Errorf("Content-Type = %#v", got)
		}
		event := decodeNIP98Header(t, r.Header.Get("Authorization"))
		if event.PubKey != admin.Public() || !event.CheckID() || !event.VerifySignature() {
			t.Error("invalid admin signature")
		}
		assertNIP98Request(t, event, serverURL(r), body, now)
		var request struct {
			Method string   `json:"method"`
			Params []string `json:"params"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		if request.Method != "allowpubkey" || len(request.Params) != 2 || request.Params[0] != pubkey.Hex() || request.Params[1] != "oauth login" {
			t.Errorf("request = %#v", request)
		}
		w.Header().Set("Content-Type", managementMediaType)
		_, _ = io.WriteString(w, `{"result":true}`)
	}))
	defer server.Close()

	client := testManagementClient(t, server.URL+"/manage?version=1", admin, server.Client(), now)
	if err := client.AllowPubKey(context.Background(), pubkey, "oauth login"); err != nil {
		t.Fatal(err)
	}
}

func TestManagementUsesInternalTransportAndExternalSigningURL(t *testing.T) {
	admin := nostr.Generate()
	endpoint, _ := url.Parse("http://nostr-relay-management.nostr.svc:8080/manage?version=1")
	signing, _ := url.Parse("https://relay.example/manage?version=1")
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != endpoint.String() {
			t.Fatalf("transport URL = %s", request.URL)
		}
		event := decodeNIP98Header(t, request.Header.Get("Authorization"))
		var signedURL string
		for _, tag := range event.Tags {
			if len(tag) == 2 && tag[0] == "u" {
				signedURL = tag[1]
			}
		}
		if signedURL != signing.String() {
			t.Fatalf("signed URL = %q", signedURL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"result":true}`)), Header: make(http.Header)}, nil
	})
	client := &HTTPManagementClient{Endpoint: endpoint, SigningURL: signing, AdminKey: admin, HTTPClient: &http.Client{Transport: transport}}
	if err := client.AllowPubKey(context.Background(), nostr.Generate().Public(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestManagementRejectsSigningPathOrQueryMismatch(t *testing.T) {
	endpoint, _ := url.Parse("http://relay.internal/manage?version=1")
	for _, raw := range []string{"https://relay.example/other?version=1", "https://relay.example/manage?version=2"} {
		signing, _ := url.Parse(raw)
		client := &HTTPManagementClient{Endpoint: endpoint, SigningURL: signing, AdminKey: nostr.Generate()}
		if err := client.AllowPubKey(context.Background(), nostr.Generate().Public(), ""); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestManagementOptionalReasonAndUnallowParams(t *testing.T) {
	admin := nostr.Generate()
	pubkey := nostr.Generate().Public()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			Method string   `json:"method"`
			Params []string `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		wantMethod := "allowpubkey"
		wantParams := []string{pubkey.Hex()}
		switch calls {
		case 2:
			wantMethod = "unallowpubkey"
			wantParams = append(wantParams, "account removed")
		case 3:
			wantMethod = "unallowpubkey"
		}
		if request.Method != wantMethod || len(request.Params) != len(wantParams) {
			t.Errorf("request %d = %#v", calls, request)
			return
		}
		for index := range wantParams {
			if request.Params[index] != wantParams[index] {
				t.Errorf("request %d = %#v", calls, request)
			}
		}
		_, _ = io.WriteString(w, `{"result":true}`)
	}))
	defer server.Close()
	client := testManagementClient(t, server.URL, admin, server.Client(), time.Now())
	if err := client.AllowPubKey(context.Background(), pubkey, ""); err != nil {
		t.Fatal(err)
	}
	if err := client.UnallowPubKey(context.Background(), pubkey, "account removed"); err != nil {
		t.Fatal(err)
	}
	if err := client.UnallowPubKey(context.Background(), pubkey, ""); err != nil {
		t.Fatal(err)
	}
}

func TestManagementSanitizesTransportErrors(t *testing.T) {
	admin := nostr.Generate()
	reason := "private removal reason"
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "arbitrary", err: errors.New("malicious transport"), want: nil},
		{name: "canceled", err: fmt.Errorf("malicious: %w", context.Canceled), want: context.Canceled},
		{name: "deadline", err: fmt.Errorf("malicious: %w", context.DeadlineExceeded), want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			var exactBody string
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				exactBody = string(body)
				return nil, fmt.Errorf("%w authorization=%s body=%s secret=%s", test.err, request.Header.Get("Authorization"), body, admin.Hex())
			})
			client := testManagementClient(t, "https://relay.example/manage", admin, &http.Client{Transport: transport}, time.Now())
			err := client.UnallowPubKey(context.Background(), nostr.Generate().Public(), reason)
			if err == nil {
				t.Fatal("expected transport error")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want sentinel %v", err, test.want)
			}
			if test.want == nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				t.Fatalf("unexpected context classification: %v", err)
			}
			for _, leaked := range []string{admin.Hex(), rAuthorizationPrefix, reason, exactBody, "malicious transport", "authorization=", "body="} {
				if strings.Contains(err.Error(), leaked) {
					t.Fatalf("error leaks %q: %v", leaked, err)
				}
			}
		})
	}
}

func TestManagementRejectsInvalidPubKeyWithoutRequest(t *testing.T) {
	var calls int
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected request")
	})
	client := testManagementClient(t, "https://relay.example/manage", nostr.Generate(), &http.Client{Transport: transport}, time.Now())
	invalidPoint := nostr.PubKey{}
	for index := range invalidPoint {
		invalidPoint[index] = 0xff
	}
	invalid := []nostr.PubKey{{}, invalidPoint}
	for _, pubkey := range invalid {
		if err := client.AllowPubKey(context.Background(), pubkey, "reason"); err == nil {
			t.Errorf("AllowPubKey(%s) succeeded", pubkey.Hex())
		}
		if err := client.UnallowPubKey(context.Background(), pubkey, "reason"); err == nil {
			t.Errorf("UnallowPubKey(%s) succeeded", pubkey.Hex())
		}
	}
	if calls != 0 {
		t.Fatalf("HTTP calls = %d, want 0", calls)
	}
}

func TestManagementReportsSafeResponseErrors(t *testing.T) {
	admin := nostr.Generate()
	secret := admin.Hex()
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "status", status: http.StatusForbidden, body: `classified response`, want: "status 403"},
		{name: "rpc error", status: http.StatusOK, body: `{"error":"private payload"}`, want: "relay management error"},
		{name: "malformed", status: http.StatusOK, body: `{`, want: "decode"},
		{name: "missing result", status: http.StatusOK, body: `{}`, want: "invalid response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			err := testManagementClient(t, server.URL, admin, server.Client(), time.Now()).AllowPubKey(context.Background(), nostr.Generate().Public(), "private payload")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			for _, leaked := range []string{secret, rAuthorizationPrefix, "private payload", "classified response"} {
				if strings.Contains(err.Error(), leaked) {
					t.Fatalf("error leaks %q: %v", leaked, err)
				}
			}
		})
	}
}

func TestManagementBoundsResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", managementResponseLimit+1))
	}))
	defer server.Close()
	err := testManagementClient(t, server.URL, nostr.Generate(), server.Client(), time.Now()).UnallowPubKey(context.Background(), nostr.Generate().Public(), "")
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("error = %v", err)
	}
}

func TestManagementHonorsContextAndHTTPTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, `{"result":true}`)
	}))
	defer server.Close()
	for _, test := range []struct {
		name   string
		client *http.Client
		ctx    func() (context.Context, context.CancelFunc)
		want   error
	}{
		{name: "context", client: server.Client(), ctx: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 20*time.Millisecond)
		}, want: context.DeadlineExceeded},
		{name: "HTTP client timeout", client: &http.Client{Timeout: 20 * time.Millisecond}, ctx: func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := test.ctx()
			defer cancel()
			err := testManagementClient(t, server.URL, nostr.Generate(), test.client, time.Now()).UnallowPubKey(ctx, nostr.Generate().Public(), "")
			if err == nil {
				t.Fatal("expected timeout error")
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want sentinel %v", err, test.want)
			}
		})
	}
}

func TestManagementRejectsInvalidConfiguration(t *testing.T) {
	valid, _ := url.Parse("https://relay.example/manage")
	for _, client := range []*HTTPManagementClient{{}, {Endpoint: valid}, {Endpoint: &url.URL{Path: "/relative"}, AdminKey: nostr.Generate()}} {
		if err := client.UnallowPubKey(context.Background(), nostr.Generate().Public(), ""); err == nil {
			t.Fatal("expected configuration error")
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

const rAuthorizationPrefix = "Nostr "

func testManagementClient(t *testing.T, endpoint string, admin nostr.SecretKey, httpClient *http.Client, now time.Time) *HTTPManagementClient {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return &HTTPManagementClient{Endpoint: u, AdminKey: admin, HTTPClient: httpClient, Now: func() time.Time { return now }}
}

func serverURL(r *http.Request) string { return "http://" + r.Host + r.URL.RequestURI() }

func assertNIP98Request(t *testing.T, event nostr.Event, requestURL string, body []byte, now time.Time) {
	t.Helper()
	if event.CreatedAt != nostr.Timestamp(now.Unix()) {
		t.Errorf("created_at = %d", event.CreatedAt)
	}
	want := map[string]string{"u": requestURL, "method": http.MethodPost}
	for _, tag := range event.Tags {
		if len(tag) == 2 {
			if value, ok := want[tag[0]]; ok && value != tag[1] {
				t.Errorf("tag %s = %q", tag[0], tag[1])
			}
			delete(want, tag[0])
		}
	}
	if len(want) != 0 {
		t.Errorf("missing tags %#v", want)
	}
	// Re-signing the observed exact body independently verifies the payload binding.
	header, err := signNIP98(requestURL, http.MethodPost, body, nostr.SecretKey{}, now)
	if !errors.Is(err, errInvalidAdminKey) || header != "" {
		t.Fatalf("zero-key check = %q, %v", header, err)
	}
	var payload string
	for _, tag := range event.Tags {
		if len(tag) == 2 && tag[0] == "payload" {
			payload = tag[1]
		}
	}
	if payload != payloadHash(body) {
		t.Errorf("payload tag = %q, want %q", payload, payloadHash(body))
	}
}
