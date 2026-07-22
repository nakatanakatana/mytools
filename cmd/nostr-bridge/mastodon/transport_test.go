package mastodon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"syscall"
	"testing"

	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

func TestDialResolvedIPPrefersIPv4(t *testing.T) {
	var attempts []string
	want, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })

	got, err := dialResolvedIP(
		context.Background(),
		"tcp",
		"social.example:443",
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{
				{IP: net.ParseIP("2001:db8::1")},
				{IP: net.ParseIP("192.0.2.1")},
			}, nil
		},
		func(_ context.Context, network, address string) (net.Conn, error) {
			attempts = append(attempts, network+" "+address)
			if network == "tcp4" {
				return want, nil
			}
			return nil, syscall.ENETUNREACH
		},
	)
	if err != nil {
		t.Fatalf("dialResolvedIP() error = %v", err)
	}
	t.Cleanup(func() { _ = got.Close() })
	if got != want {
		t.Fatal("dialResolvedIP() did not return the IPv4 connection")
	}
	if wantAttempts := []string{"tcp4 192.0.2.1:443"}; !reflect.DeepEqual(attempts, wantAttempts) {
		t.Fatalf("attempts = %#v, want %#v", attempts, wantAttempts)
	}
}

func TestDialResolvedIPFallsBackToIPv6(t *testing.T) {
	var attempts []string
	want, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })

	got, err := dialResolvedIP(
		context.Background(),
		"tcp",
		"social.example:443",
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{
				{IP: net.ParseIP("2001:db8::1")},
				{IP: net.ParseIP("192.0.2.1")},
			}, nil
		},
		func(_ context.Context, network, address string) (net.Conn, error) {
			attempts = append(attempts, network+" "+address)
			if network == "tcp6" {
				return want, nil
			}
			return nil, errors.New("IPv4 unavailable")
		},
	)
	if err != nil {
		t.Fatalf("dialResolvedIP() error = %v", err)
	}
	t.Cleanup(func() { _ = got.Close() })
	if got != want {
		t.Fatal("dialResolvedIP() did not return the IPv6 connection")
	}
	if wantAttempts := []string{"tcp4 192.0.2.1:443", "tcp6 [2001:db8::1]:443"}; !reflect.DeepEqual(attempts, wantAttempts) {
		t.Fatalf("attempts = %#v, want %#v", attempts, wantAttempts)
	}
}

func TestDialResolvedIPPerAttemptTimeout(t *testing.T) {
	var receivedDeadline bool
	want, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })

	got, err := dialResolvedIP(
		context.Background(),
		"tcp",
		"social.example:443",
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{
				{IP: net.ParseIP("192.0.2.1")},
			}, nil
		},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			if _, ok := ctx.Deadline(); ok {
				receivedDeadline = true
			}
			return want, nil
		},
	)
	if err != nil {
		t.Fatalf("dialResolvedIP() error = %v", err)
	}
	t.Cleanup(func() { _ = got.Close() })
	if !receivedDeadline {
		t.Fatal("dial Context did not receive attempt deadline")
	}
}

func TestDialResolvedIPFallsBackToIPv6OnAttemptTimeout(t *testing.T) {
	var attempts []string
	want, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })

	got, err := dialResolvedIP(
		context.Background(),
		"tcp",
		"social.example:443",
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{
				{IP: net.ParseIP("192.0.2.1")},
				{IP: net.ParseIP("2001:db8::1")},
			}, nil
		},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			attempts = append(attempts, network+" "+address)
			if network == "tcp4" {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			if network == "tcp6" {
				return want, nil
			}
			return nil, errors.New("unexpected network")
		},
	)
	if err != nil {
		t.Fatalf("dialResolvedIP() error = %v", err)
	}
	t.Cleanup(func() { _ = got.Close() })
	if got != want {
		t.Fatal("dialResolvedIP() did not fallback to IPv6 after IPv4 attempt timeout")
	}
	if wantAttempts := []string{"tcp4 192.0.2.1:443", "tcp6 [2001:db8::1]:443"}; !reflect.DeepEqual(attempts, wantAttempts) {
		t.Fatalf("attempts = %#v, want %#v", attempts, wantAttempts)
	}
}

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

	t.Run("default transport installed when HTTPClient is nil", func(t *testing.T) {
		c, err := NewOAuthClient(opts)
		if err != nil {
			t.Fatalf("NewOAuthClient() error = %v", err)
		}
		if c.httpClient == nil {
			t.Fatal("c.httpClient is nil")
		}
		if c.httpClient == http.DefaultClient {
			t.Fatal("c.httpClient should not be http.DefaultClient when nil option supplied")
		}
		if c.httpClient.Transport == nil {
			t.Fatal("c.httpClient.Transport is nil")
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

	t.Run("default transport installed when HTTPClient is nil", func(t *testing.T) {
		c, err := NewClient(opts)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		if c.http == nil {
			t.Fatal("c.http is nil")
		}
		if c.http == http.DefaultClient {
			t.Fatal("c.http should not be http.DefaultClient when nil option supplied")
		}
		if c.http.Transport == nil {
			t.Fatal("c.http.Transport is nil")
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
