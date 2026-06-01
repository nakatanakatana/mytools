package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/awnumar/memguard"
)

const (
	defaultSecretCacheTTL      = 60 * time.Second
	defaultAuthCheckMinSpacing = 5 * time.Second
	defaultAuthCheckTimeout    = 2 * time.Second
)

type BackendOptions struct {
	CacheSecrets        bool
	CacheMetadata       bool
	AsyncSave           bool
	SecretCacheTTL      time.Duration
	AuthCheckMinSpacing time.Duration
	AuthCheckTimeout    time.Duration
	Now                 func() time.Time
}

type CachedBackend struct {
	raw  RawStorageBackend
	opts BackendOptions

	metaMu     sync.RWMutex
	metaCache  map[string]*SecretItem
	metaLoaded bool
	idAliases  map[string]string

	secretMu             sync.Mutex
	secretCache          map[string]*cachedSecretItem
	authCheckInFlight    bool
	authCheckLastStarted time.Time
}

type cachedSecretItem struct {
	id         string
	label      string
	attributes map[string]string
	secret     *memguard.LockedBuffer
	expiresAt  time.Time
}

func NewCachedBackend(raw RawStorageBackend, opts BackendOptions) *CachedBackend {
	if opts.SecretCacheTTL == 0 {
		opts.SecretCacheTTL = defaultSecretCacheTTL
	}
	if opts.AuthCheckMinSpacing == 0 {
		opts.AuthCheckMinSpacing = defaultAuthCheckMinSpacing
	}
	if opts.AuthCheckTimeout == 0 {
		opts.AuthCheckTimeout = defaultAuthCheckTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &CachedBackend{
		raw:         raw,
		opts:        opts,
		metaCache:   make(map[string]*SecretItem),
		idAliases:   make(map[string]string),
		secretCache: make(map[string]*cachedSecretItem),
	}
}

func (b *CachedBackend) Search(ctx context.Context, attributes map[string]string) ([]*SecretItem, error) {
	items, err := b.metadata(ctx)
	if err != nil {
		return nil, err
	}

	matched := make([]*SecretItem, 0)
	for _, item := range items {
		if attributesMatch(item.Attributes, attributes) {
			copied := copySecretItem(item)
			copied.Secret = nil
			matched = append(matched, copied)
		}
	}
	return matched, nil
}

func (b *CachedBackend) Get(ctx context.Context, id string) (*SecretItem, error) {
	id = b.resolveID(id)
	if b.opts.CacheSecrets {
		if item, ok := b.getCachedSecret(id); ok {
			b.checkAuthAfterCacheAccess()
			return item, nil
		}
	}

	item, err := b.raw.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if b.opts.CacheSecrets {
		b.storeSecretCache(item)
	}
	return copySecretItem(item), nil
}

func (b *CachedBackend) Save(ctx context.Context, item *SecretItem) error {
	if !b.opts.AsyncSave {
		if err := b.raw.Save(ctx, item); err != nil {
			return err
		}
		b.cacheSavedItem(item)
		return nil
	}

	pendingID := ""
	persistItem := copySecretItem(item)
	if item.ID == "" {
		id, err := newPendingID()
		if err != nil {
			return err
		}
		pendingID = id
		item.ID = pendingID
		persistItem.ID = ""
	}

	b.cacheSavedItem(item)
	go b.persistSave(context.Background(), pendingID, persistItem)
	return nil
}

func (b *CachedBackend) Delete(ctx context.Context, id string) error {
	id = b.resolveID(id)
	if err := b.raw.Delete(ctx, id); err != nil {
		return err
	}

	b.metaMu.Lock()
	delete(b.metaCache, id)
	b.metaMu.Unlock()
	b.deleteSecretCache(id)
	return nil
}

func (b *CachedBackend) List(ctx context.Context) ([]*SecretItem, error) {
	items, err := b.metadata(ctx)
	if err != nil {
		return nil, err
	}

	results := make([]*SecretItem, 0, len(items))
	for _, item := range items {
		copied := copySecretItem(item)
		copied.Secret = nil
		results = append(results, copied)
	}
	return results, nil
}

func (b *CachedBackend) metadata(ctx context.Context) ([]*SecretItem, error) {
	if !b.opts.CacheMetadata {
		return b.raw.LoadMetadata(ctx)
	}

	b.metaMu.RLock()
	if b.metaLoaded {
		items := copySecretItemsWithoutSecrets(b.metaCache)
		b.metaMu.RUnlock()
		return items, nil
	}
	b.metaMu.RUnlock()

	items, err := b.raw.LoadMetadata(ctx)
	if err != nil {
		return nil, err
	}

	b.metaMu.Lock()
	b.metaCache = make(map[string]*SecretItem, len(items))
	for _, item := range items {
		copied := copySecretItem(item)
		copied.Secret = nil
		b.metaCache[copied.ID] = copied
	}
	b.metaLoaded = true
	items = copySecretItemsWithoutSecrets(b.metaCache)
	b.metaMu.Unlock()
	return items, nil
}

func (b *CachedBackend) cacheSavedItem(item *SecretItem) {
	if item == nil || item.ID == "" {
		return
	}
	if b.opts.CacheMetadata {
		copied := copySecretItem(item)
		copied.Secret = nil

		b.metaMu.Lock()
		b.metaCache[item.ID] = copied
		b.metaLoaded = true
		b.metaMu.Unlock()
	}
	if b.opts.CacheSecrets {
		b.storeSecretCache(item)
	}
}

func (b *CachedBackend) persistSave(ctx context.Context, pendingID string, item *SecretItem) {
	if err := b.raw.Save(ctx, item); err != nil {
		log.Printf("failed to persist secret: %v", err)
		return
	}
	if pendingID != "" && item.ID != "" && item.ID != pendingID {
		b.reconcileCreatedItem(pendingID, item.ID, item)
	}
}

func (b *CachedBackend) reconcileCreatedItem(pendingID, realID string, item *SecretItem) {
	b.metaMu.Lock()
	if cached := b.metaCache[pendingID]; cached != nil {
		delete(b.metaCache, pendingID)
		cached.ID = realID
		b.metaCache[realID] = cached
	} else if item != nil {
		copied := copySecretItem(item)
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

func (b *CachedBackend) resolveID(id string) string {
	b.metaMu.RLock()
	defer b.metaMu.RUnlock()
	if realID, ok := b.idAliases[id]; ok {
		return realID
	}
	return id
}

func (b *CachedBackend) getCachedSecret(id string) (*SecretItem, bool) {
	b.secretMu.Lock()
	defer b.secretMu.Unlock()

	entry, ok := b.secretCache[id]
	if !ok {
		return nil, false
	}
	now := b.opts.Now()
	if !now.Before(entry.expiresAt) || entry.secret == nil || !entry.secret.IsAlive() {
		entry.destroy()
		delete(b.secretCache, id)
		return nil, false
	}

	entry.expiresAt = now.Add(b.opts.SecretCacheTTL)
	return entry.toSecretItem(), true
}

func (b *CachedBackend) storeSecretCache(item *SecretItem) {
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
		expiresAt:  b.opts.Now().Add(b.opts.SecretCacheTTL),
	}

	b.secretMu.Lock()
	if old := b.secretCache[item.ID]; old != nil {
		old.destroy()
	}
	b.secretCache[item.ID] = entry
	b.secretMu.Unlock()
}

func (b *CachedBackend) deleteSecretCache(id string) {
	b.secretMu.Lock()
	if entry := b.secretCache[id]; entry != nil {
		entry.destroy()
		delete(b.secretCache, id)
	}
	b.secretMu.Unlock()
}

func (b *CachedBackend) clearSecretCache() {
	b.secretMu.Lock()
	for id, entry := range b.secretCache {
		entry.destroy()
		delete(b.secretCache, id)
	}
	b.secretMu.Unlock()
}

func (b *CachedBackend) checkAuthAfterCacheAccess() {
	checker, ok := b.raw.(AuthChecker)
	if !ok {
		return
	}

	b.secretMu.Lock()
	now := b.opts.Now()
	if b.authCheckInFlight || now.Sub(b.authCheckLastStarted) < b.opts.AuthCheckMinSpacing {
		b.secretMu.Unlock()
		return
	}
	b.authCheckInFlight = true
	b.authCheckLastStarted = now
	timeout := b.opts.AuthCheckTimeout
	b.secretMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		if err := checker.CheckAuth(ctx); err != nil {
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

func copySecretItemsWithoutSecrets(src map[string]*SecretItem) []*SecretItem {
	items := make([]*SecretItem, 0, len(src))
	for _, item := range src {
		copied := copySecretItem(item)
		copied.Secret = nil
		items = append(items, copied)
	}
	return items
}
