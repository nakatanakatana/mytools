package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/caarlos0/env/v11"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

// opItem is the JSON structure returned by the 'op' CLI.
type opItem struct {
	ID       string        `json:"id,omitempty"`
	Title    string        `json:"title,omitempty"`
	Category string        `json:"category,omitempty"`
	Fields   []opItemField `json:"fields,omitempty"`
}

type opItemField struct {
	ID      string `json:"id,omitempty"`
	Label   string `json:"label,omitempty"`
	Type    string `json:"type,omitempty"`
	Purpose string `json:"purpose,omitempty"`
	Value   string `json:"value,omitempty"`
}

// opListItem is the JSON structure returned by 'op item list'.
type opListItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// OnePasswordBackend implements RawStorageBackend by executing the 1Password CLI (op.exe / op).
type OnePasswordBackend struct {
	binary string
	vault  string

	authCacheTTL time.Duration
	authMu       sync.Mutex
	authLastOK   time.Time
	authFlight   singleflight.Group

	// runCmd is used to execute external commands. Placed here to allow mocking in tests.
	runCmd func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error)
}

type opConfig struct {
	Vault        string        `env:"OP_VAULT" envDefault:"wsl-keyring"`
	OPBinary     string        `env:"OP_BINARY" envDefault:"op.exe"`
	AuthCacheTTL time.Duration `env:"OP_AUTH_CACHE_TTL" envDefault:"30s"`
}

// NewOnePasswordBackend creates a new OnePasswordBackend by parsing active configurations from environment variables.
func NewOnePasswordBackend() (*OnePasswordBackend, error) {
	var cfg opConfig
	if err := env.Parse(&cfg); err != nil {
		return nil, err
	}
	return &OnePasswordBackend{
		binary:       cfg.OPBinary,
		vault:        cfg.Vault,
		authCacheTTL: cfg.AuthCacheTTL,
		runCmd: func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			if stdin != "" {
				cmd.Stdin = strings.NewReader(stdin)
			}
			return cmd.Output()
		},
	}, nil
}

func (b *OnePasswordBackend) runOP(ctx context.Context, args ...string) ([]byte, error) {
	return b.runOPWithInput(ctx, "", args...)
}

func (b *OnePasswordBackend) ensureAuthenticated(ctx context.Context) error {
	if b.authRecentlySucceeded() {
		return nil
	}

	_, err, _ := b.authFlight.Do("op-auth", func() (any, error) {
		if b.authRecentlySucceeded() {
			return nil, nil
		}
		if _, err := b.runOPNoVault(ctx, "whoami", "--format", "json"); err != nil {
			return nil, err
		}
		b.markAuthSucceeded()
		return nil, nil
	})
	return err
}

func (b *OnePasswordBackend) authRecentlySucceeded() bool {
	if b.authCacheTTL <= 0 {
		return false
	}

	b.authMu.Lock()
	defer b.authMu.Unlock()
	return !b.authLastOK.IsZero() && time.Since(b.authLastOK) < b.authCacheTTL
}

func (b *OnePasswordBackend) markAuthSucceeded() {
	b.authMu.Lock()
	b.authLastOK = time.Now()
	b.authMu.Unlock()
}

func (b *OnePasswordBackend) runOPWithInput(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	if err := b.ensureAuthenticated(ctx); err != nil {
		return nil, err
	}

	if b.vault != "" {
		args = append(args, "--vault", b.vault)
	}
	out, err := b.runCmd(ctx, stdin, b.binary, args...)
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("op command failed: %w", err)
		}
		return nil, fmt.Errorf("failed to run op: %w", err)
	}
	return out, nil
}

func (b *OnePasswordBackend) runOPNoVault(ctx context.Context, args ...string) ([]byte, error) {
	out, err := b.runCmd(ctx, "", b.binary, args...)
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("op command failed: %w", err)
		}
		return nil, fmt.Errorf("failed to run op: %w", err)
	}
	return out, nil
}

func (b *OnePasswordBackend) LoadMetadata(ctx context.Context) ([]*SecretItem, error) {
	out, err := b.runOP(ctx, "item", "list", "--tags", "wsl-keyring", "--format", "json")
	if err != nil {
		return nil, err
	}

	var list []opListItem
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}

	g, ctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, 5)
	var mu sync.Mutex
	items := make([]*SecretItem, 0, len(list))

	for _, listItem := range list {
		listItem := listItem
		g.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			item, err := b.getMetadataOnly(ctx, listItem.ID)
			if err != nil {
				return err
			}

			mu.Lock()
			items = append(items, item)
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return items, nil
}

func (b *OnePasswordBackend) getMetadataOnly(ctx context.Context, id string) (*SecretItem, error) {
	out, err := b.runOP(ctx, "item", "get", id, "--format", "json")
	if err != nil {
		return nil, err
	}

	var opIt opItem
	if err := json.Unmarshal(out, &opIt); err != nil {
		return nil, err
	}

	item := &SecretItem{
		ID:         opIt.ID,
		Label:      opIt.Title,
		Attributes: make(map[string]string),
		Secret:     nil, // explicitly nil
	}

	for _, field := range opIt.Fields {
		if field.ID == "username" {
			item.Attributes["username"] = field.Value
		} else if field.ID == "attributes" || field.Label == "attributes" {
			var attrs map[string]string
			if err := json.Unmarshal([]byte(field.Value), &attrs); err == nil {
				for k, v := range attrs {
					item.Attributes[k] = v
				}
			} else {
				if values, err := url.ParseQuery(field.Value); err == nil {
					for k, vs := range values {
						if len(vs) > 0 {
							item.Attributes[k] = vs[0]
						}
					}
				}
			}
		}
	}

	return item, nil
}

func (b *OnePasswordBackend) Search(ctx context.Context, attributes map[string]string) ([]*SecretItem, error) {
	items, err := b.LoadMetadata(ctx)
	if err != nil {
		return nil, err
	}

	var matched []*SecretItem
	for _, item := range items {
		if attributesMatch(item.Attributes, attributes) {
			copied := copySecretItem(item)
			copied.Secret = nil
			matched = append(matched, copied)
		}
	}

	return matched, nil
}

func (b *OnePasswordBackend) Get(ctx context.Context, id string) (*SecretItem, error) {
	out, err := b.runOP(ctx, "item", "get", id, "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, err.Error())
	}

	var opIt opItem
	if err := json.Unmarshal(out, &opIt); err != nil {
		return nil, fmt.Errorf("failed to parse op item: %w", err)
	}

	item := &SecretItem{
		ID:         opIt.ID,
		Label:      opIt.Title,
		Attributes: make(map[string]string),
	}

	for _, field := range opIt.Fields {
		if field.ID == "username" {
			item.Attributes["username"] = field.Value
		} else if field.ID == "password" {
			item.Secret = []byte(field.Value)
		} else if field.ID == "attributes" || field.Label == "attributes" {
			var attrs map[string]string
			if err := json.Unmarshal([]byte(field.Value), &attrs); err == nil {
				for k, v := range attrs {
					item.Attributes[k] = v
				}
			} else {
				if values, err := url.ParseQuery(field.Value); err == nil {
					for k, vs := range values {
						if len(vs) > 0 {
							item.Attributes[k] = vs[0]
						}
					}
				}
			}
		}
	}

	return item, nil
}

func (b *OnePasswordBackend) Save(ctx context.Context, item *SecretItem) error {
	values := url.Values{}
	for k, v := range item.Attributes {
		values.Set(k, v)
	}
	attrsStr := values.Encode()

	template, err := json.Marshal(buildOPItemTemplate(item, attrsStr))
	if err != nil {
		return fmt.Errorf("failed to build op item template: %w", err)
	}

	if item.ID == "" {
		if existingID, ok := b.findMatchingOPItem(ctx, item.Attributes); ok {
			args := []string{
				"item",
				"edit",
				existingID,
				"--title", item.Label,
				"--format", "json",
			}
			if _, err := b.runOPWithInput(ctx, string(template), args...); err != nil {
				return fmt.Errorf("failed to persist secret update in 1Password: %w", err)
			}
			item.ID = existingID
			return nil
		}

		args := []string{
			"item",
			"create",
			"-",
			"--tags", "wsl-keyring",
			"--title", item.Label,
			"--format", "json",
		}

		out, err := b.runOPWithInput(ctx, string(template), args...)
		if err != nil {
			return fmt.Errorf("failed to persist secret in 1Password: %w", err)
		}

		var opIt opItem
		if err := json.Unmarshal(out, &opIt); err != nil {
			return fmt.Errorf("failed to parse created 1Password item: %w", err)
		}
		item.ID = opIt.ID
		return nil
	}

	args := []string{
		"item",
		"edit",
		item.ID,
		"--title", item.Label,
		"--format", "json",
	}

	if _, err := b.runOPWithInput(ctx, string(template), args...); err != nil {
		return fmt.Errorf("failed to persist secret update in 1Password: %w", err)
	}
	return nil
}

func (b *OnePasswordBackend) findMatchingOPItem(ctx context.Context, attributes map[string]string) (string, bool) {
	out, err := b.runOP(ctx, "item", "list", "--tags", "wsl-keyring", "--format", "json")
	if err != nil {
		log.Printf("failed to list 1Password items before upsert: %v", err)
		return "", false
	}

	var list []opListItem
	if err := json.Unmarshal(out, &list); err != nil {
		log.Printf("failed to parse 1Password item list before upsert: %v", err)
		return "", false
	}

	for _, listItem := range list {
		item, err := b.getMetadataOnly(ctx, listItem.ID)
		if err != nil {
			log.Printf("failed to inspect 1Password item before upsert: %v", err)
			continue
		}
		if attributesMatch(item.Attributes, attributes) {
			return item.ID, true
		}
	}
	return "", false
}

func buildOPItemTemplate(item *SecretItem, attrsStr string) opItem {
	return opItem{
		Title:    item.Label,
		Category: "LOGIN",
		Fields: []opItemField{
			{
				ID:      "username",
				Label:   "username",
				Type:    "STRING",
				Purpose: "USERNAME",
				Value:   item.Attributes["username"],
			},
			{
				ID:      "password",
				Label:   "password",
				Type:    "CONCEALED",
				Purpose: "PASSWORD",
				Value:   string(item.Secret),
			},
			{
				ID:    "attributes",
				Label: "attributes",
				Type:  "STRING",
				Value: attrsStr,
			},
		},
	}
}

func (b *OnePasswordBackend) Delete(ctx context.Context, id string) error {
	_, err := b.runOP(ctx, "item", "delete", id)
	return err
}

func (b *OnePasswordBackend) List(ctx context.Context) ([]*SecretItem, error) {
	return b.LoadMetadata(ctx)
}

func (b *OnePasswordBackend) CheckAuth(ctx context.Context) error {
	return b.ensureAuthenticated(ctx)
}
