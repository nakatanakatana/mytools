package mastodon

import (
	"context"
	"errors"
	"net/http"
	"testing"

	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

type dummyTokenSource struct{}

func (dummyTokenSource) Token(context.Context) (Token, error) {
	return Token{}, errors.New("not implemented")
}

func TestNewOAuthClientHTTPClient(t *testing.T) {
	opts := OAuthOptions{
		Store:         &memoryOAuthStore{},
		BaseURL:       "https://social.example",
		Account:       "user@social.example",
		ClientID:      "client-id",
		ClientSecret:  "client-secret",
		RedirectURL:   "https://app.example/callback",
		EncryptionKey: make([]byte, 32),
		Scope:         bridgestore.SourceScope{Provider: "mastodon", Account: "user@social.example"},
	}

	t.Run("default client used when HTTPClient is nil", func(t *testing.T) {
		c, err := NewOAuthClient(opts)
		if err != nil {
			t.Fatalf("NewOAuthClient() error = %v", err)
		}
		if c.httpClient != http.DefaultClient {
			t.Fatalf("default HTTP client = %p, want %p", c.httpClient, http.DefaultClient)
		}
	})

	t.Run("exact pointer retained when HTTPClient supplied", func(t *testing.T) {
		custom := &http.Client{}
		optsWithCustom := opts
		optsWithCustom.HTTPClient = custom
		c, err := NewOAuthClient(optsWithCustom)
		if err != nil {
			t.Fatalf("NewOAuthClient() error = %v", err)
		}
		if c.httpClient != custom {
			t.Fatalf("c.httpClient = %p, want %p", c.httpClient, custom)
		}
	})
}

func TestNewClientHTTPClient(t *testing.T) {
	opts := ClientOptions{
		BaseURL: "https://social.example",
		Tokens:  dummyTokenSource{},
	}

	t.Run("default client used when HTTPClient is nil", func(t *testing.T) {
		c, err := NewClient(opts)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		if c.http != http.DefaultClient {
			t.Fatalf("default HTTP client = %p, want %p", c.http, http.DefaultClient)
		}
	})

	t.Run("exact pointer retained when HTTPClient supplied", func(t *testing.T) {
		custom := &http.Client{}
		optsWithCustom := opts
		optsWithCustom.HTTPClient = custom
		c, err := NewClient(optsWithCustom)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		if c.http != custom {
			t.Fatalf("c.http = %p, want %p", c.http, custom)
		}
	})
}
