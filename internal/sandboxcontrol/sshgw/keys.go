package sshgw

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/ssh"
)

const (
	defaultGitHubBaseURL = "https://github.com"
	maxKeysBodyBytes     = 1 << 20 // 1 MiB
)

// KeyStoreOptions configures where authorized SSH public keys are loaded from.
type KeyStoreOptions struct {
	GitHubUsers          []string
	GitHubBaseURL        string
	GitLabUsers          []string
	GitLabBaseURL        string
	AuthorizedKeysPath   string
	AuthorizedKeysInline string
	InsecureAllowAny     bool
	HTTPClient           *http.Client
	Logger               *slog.Logger
}

// KeyStore maintains an in-memory cache of authorized SSH public keys.
type KeyStore struct {
	opts KeyStoreOptions

	mu   sync.RWMutex
	keys []ssh.PublicKey
}

// NewKeyStore creates a KeyStore with defaults applied.
func NewKeyStore(opts KeyStoreOptions) *KeyStore {
	if opts.GitHubBaseURL == "" {
		opts.GitHubBaseURL = defaultGitHubBaseURL
	}
	if opts.GitLabBaseURL == "" {
		opts.GitLabBaseURL = "https://gitlab.com"
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &KeyStore{opts: opts}
}

// Refresh reloads public keys from all configured static and remote sources.
// If remote fetches transiently fail and a previous cache exists, the previous cache is retained.
func (ks *KeyStore) Refresh(ctx context.Context) error {
	var (
		collected []ssh.PublicKey
		errs      []error
	)

	if strings.TrimSpace(ks.opts.AuthorizedKeysInline) != "" {
		parsed := parseAuthorizedKeys([]byte(ks.opts.AuthorizedKeysInline))
		collected = append(collected, parsed...)
	}

	if path := strings.TrimSpace(ks.opts.AuthorizedKeysPath); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("read authorized_keys file %q: %w", path, err))
		} else {
			collected = append(collected, parseAuthorizedKeys(raw)...)
		}
	}

	for _, user := range ks.opts.GitHubUsers {
		u := fmt.Sprintf("%s/%s.keys", strings.TrimRight(ks.opts.GitHubBaseURL, "/"), user)
		keys, err := ks.fetchRemoteKeys(ctx, u)
		if err != nil {
			errs = append(errs, fmt.Errorf("fetch GitHub keys for %q: %w", user, err))
			continue
		}
		collected = append(collected, keys...)
	}

	for _, user := range ks.opts.GitLabUsers {
		u := fmt.Sprintf("%s/%s.keys", strings.TrimRight(ks.opts.GitLabBaseURL, "/"), user)
		keys, err := ks.fetchRemoteKeys(ctx, u)
		if err != nil {
			errs = append(errs, fmt.Errorf("fetch GitLab keys for %q: %w", user, err))
			continue
		}
		collected = append(collected, keys...)
	}

	ks.mu.Lock()
	defer ks.mu.Unlock()

	if len(collected) > 0 {
		ks.keys = dedupeKeys(collected)
		if len(errs) > 0 {
			ks.opts.Logger.Warn("partial failure refreshing ssh keys; updated with available keys", "count", len(ks.keys), "error", errors.Join(errs...))
		}
		return nil
	}

	if len(ks.keys) > 0 {
		ks.opts.Logger.Warn("ssh key refresh yielded no keys; retaining cached keys", "cached", len(ks.keys), "error", errors.Join(errs...))
		return errors.Join(errs...)
	}

	if ks.opts.InsecureAllowAny {
		return nil
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return errors.New("no authorized SSH public keys loaded from any source")
}

// StartRefreshLoop periodically refreshes keys until ctx is cancelled.
func (ks *KeyStore) StartRefreshLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_ = ks.Refresh(refreshCtx)
			cancel()
		}
	}
}

// Authorize checks whether key matches any currently cached authorized public key.
func (ks *KeyStore) Authorize(key ssh.PublicKey) bool {
	if ks.opts.InsecureAllowAny {
		return true
	}
	if key == nil {
		return false
	}
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	for _, allowed := range ks.keys {
		if ssh.KeysEqual(key, allowed) {
			return true
		}
	}
	return false
}

// Count returns the number of currently cached authorized public keys.
func (ks *KeyStore) Count() int {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return len(ks.keys)
}

func (ks *KeyStore) fetchRemoteKeys(ctx context.Context, targetURL string) ([]ssh.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "sandbox-control/1.0")
	resp, err := ks.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, targetURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxKeysBodyBytes))
	if err != nil {
		return nil, err
	}
	keys := parseAuthorizedKeys(body)
	if len(keys) == 0 {
		return nil, fmt.Errorf("no valid SSH public keys found at %s", targetURL)
	}
	return keys, nil
}

func parseAuthorizedKeys(raw []byte) []ssh.PublicKey {
	var out []ssh.PublicKey
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			continue
		}
		out = append(out, pub)
	}
	return out
}

func dedupeKeys(in []ssh.PublicKey) []ssh.PublicKey {
	out := make([]ssh.PublicKey, 0, len(in))
	for _, k := range in {
		dup := false
		for _, existing := range out {
			if ssh.KeysEqual(k, existing) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, k)
		}
	}
	return out
}
