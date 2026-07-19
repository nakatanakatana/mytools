package mastodon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type staticTokenSource struct{ token Token }

func (s staticTokenSource) Token(context.Context) (Token, error) { return s.token, nil }

func TestClientFollowingUsesBearerAuthAndSameOriginPagination(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.RawQuery {
		case "":
			w.Header().Set("Link", `<`+server.URL+`/api/v1/accounts/me/following?max_id=2>; rel="next"`)
			_, _ = w.Write([]byte(`[{"id":"1","uri":"https://one.example/users/a"}]`))
		case "max_id=2":
			_, _ = w.Write([]byte(`[{"id":"2","uri":"https://two.example/users/b"}]`))
		default:
			t.Fatalf("unexpected query %q", r.URL.RawQuery)
		}
	}))
	defer server.Close()
	c, err := NewClient(ClientOptions{BaseURL: server.URL, Tokens: staticTokenSource{Token{AccessToken: "secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Following(context.Background(), "me")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].URI != "https://two.example/users/b" {
		t.Fatalf("following = %#v", got)
	}
}

func TestClientRejectsCrossOriginPagination(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("followed cross-origin link") }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<`+other.URL+`/steal>; rel="next"`)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	c, _ := NewClient(ClientOptions{BaseURL: server.URL, Tokens: staticTokenSource{Token{AccessToken: "secret"}}})
	if _, err := c.Following(context.Background(), "me"); err == nil {
		t.Fatal("expected cross-origin pagination error")
	}
}

func TestClientFindsNextAcrossRepeatedLinkFieldsWithQuotedComma(t *testing.T) {
	requests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.RawQuery == "" {
			w.Header().Add("Link", `<`+server.URL+`/ignored>; rel="prev"; title="before, after"`)
			w.Header().Add("Link", `<`+server.URL+`/api/v1/lists?max_id=2>; rel="next"`)
			_, _ = io.WriteString(w, `[{"id":"1"}]`)
			return
		}
		_, _ = io.WriteString(w, `[{"id":"2"}]`)
	}))
	defer server.Close()
	c, _ := NewClient(ClientOptions{BaseURL: server.URL, Tokens: staticTokenSource{Token{AccessToken: "secret"}}})
	lists, err := c.Lists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(lists) != 2 {
		t.Fatalf("requests=%d lists=%#v", requests, lists)
	}
}

func TestClientHonorsRetryAfterAndCapsRetries(t *testing.T) {
	attempts := 0
	var delays []time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "2")
		http.Error(w, "busy", http.StatusTooManyRequests)
	}))
	defer server.Close()
	c, _ := NewClient(ClientOptions{BaseURL: server.URL, Tokens: staticTokenSource{Token{AccessToken: "secret"}}, MaxRetries: 2, Sleep: func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }})
	if _, err := c.Lists(context.Background()); err == nil {
		t.Fatal("expected exhausted retry error")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if len(delays) != 2 || delays[0] != 2*time.Second {
		t.Fatalf("delays = %v", delays)
	}
}

func TestClientHonorsHTTPDateRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	attempts := 0
	var delay time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", now.Add(3*time.Second).Format(http.TimeFormat))
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	}))
	defer server.Close()
	c, _ := NewClient(ClientOptions{BaseURL: server.URL, Tokens: staticTokenSource{Token{AccessToken: "secret"}}, MaxRetries: 1, Now: func() time.Time { return now }, Sleep: func(_ context.Context, d time.Duration) error { delay = d; return nil }})
	if _, err := c.Lists(context.Background()); err != nil {
		t.Fatal(err)
	}
	if delay != 3*time.Second {
		t.Fatalf("delay = %s", delay)
	}
}

type countingTokenSource struct{ calls int }

func (s *countingTokenSource) Token(context.Context) (Token, error) {
	s.calls++
	return Token{AccessToken: fmt.Sprint(s.calls)}, nil
}

func TestClientReacquiresTokenForEveryRetryAndPage(t *testing.T) {
	tokens := &countingTokenSource{}
	requests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer "+fmt.Sprint(requests) {
			t.Fatalf("authorization = %q on request %d", r.Header.Get("Authorization"), requests)
		}
		if requests == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		if requests == 2 {
			w.Header().Set("Link", `<`+server.URL+`/api/v1/lists?max_id=2>; rel="next"`)
		}
		_, _ = io.WriteString(w, `[]`)
	}))
	defer server.Close()
	c, _ := NewClient(ClientOptions{BaseURL: server.URL, Tokens: tokens, MaxRetries: 1, Sleep: func(context.Context, time.Duration) error { return nil }})
	if _, err := c.Lists(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tokens.calls != 3 {
		t.Fatalf("token calls = %d", tokens.calls)
	}
}

func TestDecodeResponseEnforcesExactSizeAndSingleJSONValue(t *testing.T) {
	exact := append([]byte(`[]`), bytes.Repeat([]byte(" "), maxResponseBytes-2)...)
	var value []Account
	if err := decodeResponse(bytes.NewReader(exact), &value); err != nil {
		t.Fatalf("exact limit: %v", err)
	}
	oversize := append(exact, ' ')
	if err := decodeResponse(bytes.NewReader(oversize), &value); err == nil {
		t.Fatal("oversize response accepted")
	}
	if err := decodeResponse(strings.NewReader(`[] {}`), &value); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestClientCapsPagination(t *testing.T) {
	requests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Link", `<`+server.URL+`/api/v1/lists>; rel="next"`)
		_, _ = io.WriteString(w, `[]`)
	}))
	defer server.Close()
	c, _ := NewClient(ClientOptions{BaseURL: server.URL, Tokens: staticTokenSource{Token{AccessToken: "secret"}}})
	if _, err := c.Lists(context.Background()); err == nil {
		t.Fatal("expected pagination limit error")
	}
	if requests != maxPages {
		t.Fatalf("requests = %d, want %d", requests, maxPages)
	}
}
