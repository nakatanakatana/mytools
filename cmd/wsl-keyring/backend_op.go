package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/awnumar/memguard"
	"github.com/caarlos0/env/v11"
	"golang.org/x/sync/errgroup"
)

const (
	defaultSecretCacheTTL      = 60 * time.Second
	defaultAuthCheckMinSpacing = 5 * time.Second
	defaultAuthCheckTimeout    = 2 * time.Second
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

// OnePasswordBackend implements StorageBackend by executing the 1Password CLI (op.exe / op).
type OnePasswordBackend struct {
	binary string
	vault  string

	metaCache  map[string]*SecretItem
	idAliases  map[string]string
	metaMu     sync.RWMutex
	metaLoaded bool

	secretCache          map[string]*cachedSecretItem
	secretMu             sync.Mutex
	secretCacheTTL       time.Duration
	authCheckMinSpacing  time.Duration
	authCheckTimeout     time.Duration
	authCheckInFlight    bool
	authCheckLastStarted time.Time
	now                  func() time.Time

	// runCmd is used to execute external commands. Placed here to allow mocking in tests.
	runCmd func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error)
}

type cachedSecretItem struct {
	id         string
	label      string
	attributes map[string]string
	secret     *memguard.LockedBuffer
	expiresAt  time.Time
}

type opConfig struct {
	Vault    string `env:"OP_VAULT" envDefault:"wsl-keyring"`
	OPBinary string `env:"OP_BINARY" envDefault:"op.exe"`
}

// NewOnePasswordBackend creates a new OnePasswordBackend by parsing active configurations from environment variables.
func NewOnePasswordBackend() (*OnePasswordBackend, error) {
	var cfg opConfig
	if err := env.Parse(&cfg); err != nil {
		return nil, err
	}
	return &OnePasswordBackend{
		binary:              cfg.OPBinary,
		vault:               cfg.Vault,
		metaCache:           make(map[string]*SecretItem),
		idAliases:           make(map[string]string),
		secretCache:         make(map[string]*cachedSecretItem),
		secretCacheTTL:      defaultSecretCacheTTL,
		authCheckMinSpacing: defaultAuthCheckMinSpacing,
		authCheckTimeout:    defaultAuthCheckTimeout,
		now:                 time.Now,
		runCmd: func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			if stdin != "" {
				cmd.Stdin = strings.NewReader(stdin)
			}
			return cmd.Output()
		},
	}, nil
}

func (b *OnePasswordBackend) ensureInitialized() {
	b.metaMu.Lock()
	if b.metaCache == nil {
		b.metaCache = make(map[string]*SecretItem)
	}
	if b.idAliases == nil {
		b.idAliases = make(map[string]string)
	}
	b.metaMu.Unlock()

	b.secretMu.Lock()
	if b.secretCache == nil {
		b.secretCache = make(map[string]*cachedSecretItem)
	}
	if b.secretCacheTTL == 0 {
		b.secretCacheTTL = defaultSecretCacheTTL
	}
	if b.authCheckMinSpacing == 0 {
		b.authCheckMinSpacing = defaultAuthCheckMinSpacing
	}
	if b.authCheckTimeout == 0 {
		b.authCheckTimeout = defaultAuthCheckTimeout
	}
	if b.now == nil {
		b.now = time.Now
	}
	b.secretMu.Unlock()
}

func (b *OnePasswordBackend) runOP(ctx context.Context, args ...string) ([]byte, error) {
	return b.runOPWithInput(ctx, "", args...)
}

func (b *OnePasswordBackend) runOPWithInput(ctx context.Context, stdin string, args ...string) ([]byte, error) {
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

func (b *OnePasswordBackend) loadMetaCache(ctx context.Context) error {
	b.ensureInitialized()
	b.metaMu.Lock()
	if b.metaLoaded {
		b.metaMu.Unlock()
		return nil
	}
	b.metaMu.Unlock()

	out, err := b.runOP(ctx, "item", "list", "--tags", "wsl-keyring", "--format", "json")
	if err != nil {
		return err
	}

	var list []opListItem
	if err := json.Unmarshal(out, &list); err != nil {
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, 5)
	var mu sync.Mutex
	tempCache := make(map[string]*SecretItem)

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
			tempCache[item.ID] = item
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	b.metaMu.Lock()
	b.metaCache = tempCache
	b.metaLoaded = true
	b.metaMu.Unlock()

	return nil
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
	if err := b.loadMetaCache(ctx); err != nil {
		return nil, err
	}

	b.metaMu.RLock()
	defer b.metaMu.RUnlock()

	var matched []*SecretItem
	for _, item := range b.metaCache {
		match := true
		for k, v := range attributes {
			val, ok := item.Attributes[k]
			if !ok || val != v {
				match = false
				break
			}
		}
		if match {
			copied := &SecretItem{
				ID:         item.ID,
				Label:      item.Label,
				Attributes: make(map[string]string),
				Secret:     nil,
			}
			for k, v := range item.Attributes {
				copied.Attributes[k] = v
			}
			matched = append(matched, copied)
		}
	}

	return matched, nil
}

func (b *OnePasswordBackend) Get(ctx context.Context, id string) (*SecretItem, error) {
	b.ensureInitialized()
	id = b.resolveID(id)

	if item, ok := b.getCachedSecret(id); ok {
		b.checkAuthAfterCacheAccess()
		return item, nil
	}

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

	b.storeSecretCache(item)
	return item, nil
}

func (b *OnePasswordBackend) Save(ctx context.Context, item *SecretItem) error {
	b.ensureInitialized()
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
		pendingID, err := newPendingID()
		if err != nil {
			return err
		}
		item.ID = pendingID

		args := []string{
			"item",
			"create",
			"-",
			"--tags", "wsl-keyring",
			"--title", item.Label,
			"--format", "json",
		}

		b.cacheSavedItem(item)

		persistItem := copySecretItem(item)
		go b.persistCreate(context.Background(), pendingID, persistItem, string(template), args)

		return nil
	}

	// Existing item edit: persist before returning success so callers can
	// immediately search/get without racing a background op edit.
	args := []string{
		"item",
		"edit",
		item.ID,
		"--title", item.Label,
		"--format", "json",
	}

	b.cacheSavedItem(item)

	go b.persistEdit(context.Background(), copySecretItem(item), string(template), args)

	return nil
}

func (b *OnePasswordBackend) cacheSavedItem(item *SecretItem) {
	b.ensureInitialized()
	b.metaMu.Lock()
	copiedAttrs := make(map[string]string)
	for k, v := range item.Attributes {
		copiedAttrs[k] = v
	}
	b.metaCache[item.ID] = &SecretItem{
		ID:         item.ID,
		Label:      item.Label,
		Attributes: copiedAttrs,
		Secret:     nil,
	}
	b.metaLoaded = true
	b.metaMu.Unlock()

	b.storeSecretCache(item)
}

func (b *OnePasswordBackend) persistCreate(ctx context.Context, pendingID string, item *SecretItem, template string, args []string) {
	if existingID, ok := b.findMatchingOPItem(ctx, item.Attributes); ok {
		editArgs := []string{
			"item",
			"edit",
			existingID,
			"--title", item.Label,
			"--format", "json",
		}
		if _, err := b.runOPWithInput(ctx, template, editArgs...); err != nil {
			log.Printf("failed to persist secret update in 1Password: %v", err)
			return
		}
		b.reconcileCreatedItem(pendingID, existingID, item)
		return
	}

	out, err := b.runOPWithInput(ctx, template, args...)
	if err != nil {
		log.Printf("failed to persist secret in 1Password: %v", err)
		return
	}

	var opIt opItem
	if err := json.Unmarshal(out, &opIt); err != nil {
		log.Printf("failed to parse created 1Password item: %v", err)
		return
	}
	if opIt.ID == "" || opIt.ID == pendingID {
		return
	}

	b.reconcileCreatedItem(pendingID, opIt.ID, item)
}

func (b *OnePasswordBackend) persistEdit(ctx context.Context, item *SecretItem, template string, args []string) {
	if _, err := b.runOPWithInput(ctx, template, args...); err != nil {
		log.Printf("failed to persist secret update in 1Password: %v", err)
	}
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

func (b *OnePasswordBackend) reconcileCreatedItem(pendingID, realID string, item *SecretItem) {
	b.metaMu.Lock()
	if cached := b.metaCache[pendingID]; cached != nil {
		delete(b.metaCache, pendingID)
		cached.ID = realID
		b.metaCache[realID] = cached
	} else {
		copied := copySecretItem(item)
		copied.ID = realID
		copied.Secret = nil
		b.metaCache[realID] = copied
	}
	b.idAliases[pendingID] = realID
	b.metaMu.Unlock()

	b.secretMu.Lock()
	if entry := b.secretCache[pendingID]; entry != nil {
		delete(b.secretCache, pendingID)
		entry.id = realID
		b.secretCache[realID] = entry
	}
	b.secretMu.Unlock()
}

func (b *OnePasswordBackend) resolveID(id string) string {
	b.metaMu.RLock()
	defer b.metaMu.RUnlock()
	if realID, ok := b.idAliases[id]; ok {
		return realID
	}
	return id
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
	b.ensureInitialized()
	id = b.resolveID(id)
	_, err := b.runOP(ctx, "item", "delete", id)
	if err == nil {
		b.metaMu.Lock()
		delete(b.metaCache, id)
		b.metaMu.Unlock()
		b.deleteSecretCache(id)
	}
	return err
}

func (b *OnePasswordBackend) List(ctx context.Context) ([]*SecretItem, error) {
	if err := b.loadMetaCache(ctx); err != nil {
		return nil, err
	}

	b.metaMu.RLock()
	defer b.metaMu.RUnlock()

	results := make([]*SecretItem, 0, len(b.metaCache))
	for _, item := range b.metaCache {
		copied := &SecretItem{
			ID:         item.ID,
			Label:      item.Label,
			Attributes: make(map[string]string),
			Secret:     nil,
		}
		for k, v := range item.Attributes {
			copied.Attributes[k] = v
		}
		results = append(results, copied)
	}
	return results, nil
}

func (b *OnePasswordBackend) getCachedSecret(id string) (*SecretItem, bool) {
	b.secretMu.Lock()
	defer b.secretMu.Unlock()

	entry, ok := b.secretCache[id]
	if !ok {
		return nil, false
	}
	now := b.now()
	if !now.Before(entry.expiresAt) || entry.secret == nil || !entry.secret.IsAlive() {
		entry.destroy()
		delete(b.secretCache, id)
		return nil, false
	}

	entry.expiresAt = now.Add(b.secretCacheTTL)
	return entry.toSecretItem(), true
}

func (b *OnePasswordBackend) storeSecretCache(item *SecretItem) {
	if item == nil || item.ID == "" || item.Secret == nil {
		return
	}

	secretCopy := append([]byte(nil), item.Secret...)
	buf := memguard.NewBufferFromBytes(secretCopy)
	if buf == nil || !buf.IsAlive() || buf.Size() != len(item.Secret) {
		if buf != nil {
			buf.Destroy()
		}
		return
	}

	entry := &cachedSecretItem{
		id:         item.ID,
		label:      item.Label,
		attributes: copyAttributes(item.Attributes),
		secret:     buf,
		expiresAt:  b.now().Add(b.secretCacheTTL),
	}

	b.secretMu.Lock()
	if old := b.secretCache[item.ID]; old != nil {
		old.destroy()
	}
	b.secretCache[item.ID] = entry
	b.secretMu.Unlock()
}

func (b *OnePasswordBackend) deleteSecretCache(id string) {
	b.secretMu.Lock()
	if entry := b.secretCache[id]; entry != nil {
		entry.destroy()
		delete(b.secretCache, id)
	}
	b.secretMu.Unlock()
}

func (b *OnePasswordBackend) clearSecretCache() {
	b.secretMu.Lock()
	for id, entry := range b.secretCache {
		entry.destroy()
		delete(b.secretCache, id)
	}
	b.secretMu.Unlock()
}

func (b *OnePasswordBackend) checkAuthAfterCacheAccess() {
	b.secretMu.Lock()
	now := b.now()
	if b.authCheckInFlight || now.Sub(b.authCheckLastStarted) < b.authCheckMinSpacing {
		b.secretMu.Unlock()
		return
	}
	b.authCheckInFlight = true
	b.authCheckLastStarted = now
	timeout := b.authCheckTimeout
	b.secretMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		_, err := b.runOPNoVault(ctx, "whoami", "--format", "json")
		if err != nil {
			b.clearSecretCache()
		}

		b.secretMu.Lock()
		b.authCheckInFlight = false
		b.secretMu.Unlock()
	}()
}

func (c *cachedSecretItem) toSecretItem() *SecretItem {
	return &SecretItem{
		ID:         c.id,
		Label:      c.label,
		Attributes: copyAttributes(c.attributes),
		Secret:     append([]byte(nil), c.secret.Bytes()...),
	}
}

func (c *cachedSecretItem) destroy() {
	if c.secret != nil && c.secret.IsAlive() {
		c.secret.Destroy()
	}
}

func copyAttributes(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func attributesMatch(got, want map[string]string) bool {
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func copySecretItem(src *SecretItem) *SecretItem {
	if src == nil {
		return nil
	}
	return &SecretItem{
		ID:         src.ID,
		Label:      src.Label,
		Attributes: copyAttributes(src.Attributes),
		Secret:     append([]byte(nil), src.Secret...),
	}
}

func newPendingID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate pending item ID: %w", err)
	}
	return "pending_" + hex.EncodeToString(buf), nil
}
