package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/awnumar/memguard"
	"golang.org/x/sync/singleflight"
)

const (
	defaultSecretCacheTTL      = 60 * time.Second
	defaultAuthCheckMinSpacing = 5 * time.Second
	defaultAuthCheckTimeout    = 2 * time.Second
	defaultSaveTimeout         = 30 * time.Second
)

type BackendOptions struct {
	CacheSecrets        bool
	CacheMetadata       bool
	AsyncSave           bool
	SecretCacheTTL      time.Duration
	AuthCheckMinSpacing time.Duration
	AuthCheckTimeout    time.Duration
	SaveTimeout         time.Duration
	Now                 func() time.Time
}

type CachedBackend struct {
	raw  RawStorageBackend
	opts BackendOptions

	metaMu       sync.RWMutex
	metaCache    map[string]*SecretItem
	metaLoaded   bool
	metaLoad     singleflight.Group
	idAliases    map[string]string
	pendingSaves map[string][]*pendingSaveState

	secretMu             sync.Mutex
	secretCache          map[string]*cachedSecretItem
	authCheckInFlight    bool
	authCheckLastStarted time.Time

	beforePersistSave func()
}

type pendingSaveState struct {
	done       chan struct{}
	prevDone   <-chan struct{}
	deleted    bool
	superseded bool
	isNew      bool
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
		raw:          raw,
		opts:         opts,
		metaCache:    make(map[string]*SecretItem),
		idAliases:    make(map[string]string),
		pendingSaves: make(map[string][]*pendingSaveState),
		secretCache:  make(map[string]*cachedSecretItem),
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

func (b *CachedBackend) saveTimeout() time.Duration {
	if b.opts.SaveTimeout > 0 {
		return b.opts.SaveTimeout
	}
	return defaultSaveTimeout
}

func (b *CachedBackend) Save(ctx context.Context, item *SecretItem) error {
	if item == nil {
		return errors.New("item is nil")
	}

	if !b.opts.AsyncSave {
		if item.ID != "" {
			item.ID = b.resolveID(item.ID)
			if isPendingID(item.ID) {
				item.ID = ""
			}
		}
		if err := b.raw.Save(ctx, item); err != nil {
			return err
		}
		b.cacheSavedItem(item)
		return nil
	}

	b.metaMu.Lock()
	if item.ID != "" {
		if realID, ok := b.idAliases[item.ID]; ok {
			item.ID = realID
		}
	}

	pendingID := ""
	persistItem := copySecretItem(item)
	trackingKey := item.ID
	if item.ID == "" || isPendingID(item.ID) {
		if item.ID == "" {
			id, err := newPendingID()
			if err != nil {
				b.metaMu.Unlock()
				return err
			}
			pendingID = id
			item.ID = pendingID
		} else {
			pendingID = item.ID
		}
		persistItem.ID = ""
		trackingKey = pendingID
	}

	state := &pendingSaveState{isNew: pendingID != ""}
	b.addPendingSaveLocked(trackingKey, state)
	if b.opts.CacheMetadata {
		copied := copySecretItem(item)
		copied.Secret = nil
		b.metaCache[item.ID] = copied
		b.metaLoaded = true
	}
	if b.opts.CacheSecrets {
		b.storeSecretCache(item)
	}
	b.metaMu.Unlock()

	saveCtx, cancel := context.WithTimeout(context.Background(), b.saveTimeout())
	go func() {
		defer cancel()
		b.persistSave(saveCtx, pendingID, trackingKey, state, persistItem)
	}()
	return nil
}

func (b *CachedBackend) Delete(ctx context.Context, id string) error {
	b.metaMu.Lock()
	resolvedID := id
	if realID, ok := b.idAliases[id]; ok {
		resolvedID = realID
	}

	b.markPendingSavesDeletedLocked(id)
	if resolvedID != id {
		b.markPendingSavesDeletedLocked(resolvedID)
	}
	for pending, target := range b.idAliases {
		if target == id || target == resolvedID {
			b.markPendingSavesDeletedLocked(pending)
		}
	}

	if isPendingID(resolvedID) {
		b.evictItemFromCachesLocked(id, resolvedID)
		b.metaMu.Unlock()

		b.deleteSecretCache(id)
		b.deleteSecretCache(resolvedID)
		return nil
	}
	b.metaMu.Unlock()

	if err := b.raw.Delete(ctx, resolvedID); err != nil {
		if errors.Is(err, ErrNotFound) {
			b.metaMu.Lock()
			b.evictItemFromCachesLocked(id, resolvedID)
			b.metaMu.Unlock()
			b.deleteSecretCache(id)
			b.deleteSecretCache(resolvedID)
			return nil
		}
		return err
	}

	b.metaMu.Lock()
	b.evictItemFromCachesLocked(id, resolvedID)
	b.metaMu.Unlock()
	b.deleteSecretCache(id)
	b.deleteSecretCache(resolvedID)
	return nil
}

func (b *CachedBackend) evictItemFromCachesLocked(id, resolvedID string) {
	delete(b.metaCache, id)
	delete(b.metaCache, resolvedID)
	b.cleanupAliasesLocked(id, resolvedID)
}

func (b *CachedBackend) addPendingSaveLocked(key string, state *pendingSaveState) {
	state.done = make(chan struct{})
	if existing := b.pendingSaves[key]; len(existing) > 0 {
		for _, s := range existing {
			s.superseded = true
		}
		state.prevDone = existing[len(existing)-1].done
	}
	b.pendingSaves[key] = append(b.pendingSaves[key], state)
}

func (b *CachedBackend) removePendingSaveLocked(key string, state *pendingSaveState) {
	if b.removePendingSaveFromKeyLocked(key, state) {
		return
	}
	if realID, ok := b.idAliases[key]; ok && realID != key {
		if b.removePendingSaveFromKeyLocked(realID, state) {
			return
		}
	}
	// Fallback in case aliases were already cleared by a concurrent Delete
	for k := range b.pendingSaves {
		if b.removePendingSaveFromKeyLocked(k, state) {
			break
		}
	}
}

func (b *CachedBackend) removePendingSaveFromKeyLocked(key string, state *pendingSaveState) bool {
	states := b.pendingSaves[key]
	found := false
	for i, s := range states {
		if s == state {
			copy(states[i:], states[i+1:])
			states[len(states)-1] = nil
			b.pendingSaves[key] = states[:len(states)-1]
			found = true
			break
		}
	}
	if len(b.pendingSaves[key]) == 0 {
		delete(b.pendingSaves, key)
	}
	return found
}

func (b *CachedBackend) markPendingSavesDeletedLocked(key string) bool {
	states := b.pendingSaves[key]
	if len(states) == 0 {
		return false
	}
	for _, s := range states {
		s.deleted = true
	}
	return true
}

func (b *CachedBackend) cleanupAliasesLocked(ids ...string) {
	for _, id := range ids {
		if id == "" {
			continue
		}
		delete(b.idAliases, id)
		for k, v := range b.idAliases {
			if v == id {
				delete(b.idAliases, k)
			}
		}
	}
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

	_, err, _ := b.metaLoad.Do("metadata", func() (any, error) {
		return nil, b.loadMetadataCache(ctx)
	})
	if err != nil {
		return nil, err
	}

	b.metaMu.RLock()
	items := copySecretItemsWithoutSecrets(b.metaCache)
	b.metaMu.RUnlock()
	return items, nil
}

func (b *CachedBackend) loadMetadataCache(ctx context.Context) error {
	b.metaMu.RLock()
	if b.metaLoaded {
		b.metaMu.RUnlock()
		return nil
	}
	b.metaMu.RUnlock()

	items, err := b.raw.LoadMetadata(ctx)
	if err != nil {
		return err
	}

	b.metaMu.Lock()
	b.metaCache = make(map[string]*SecretItem, len(items))
	for _, item := range items {
		copied := copySecretItem(item)
		copied.Secret = nil
		b.metaCache[copied.ID] = copied
	}
	b.metaLoaded = true
	b.metaMu.Unlock()
	return nil
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

func (b *CachedBackend) persistSave(ctx context.Context, pendingID, trackingKey string, state *pendingSaveState, item *SecretItem) {
	defer close(state.done)

	if state.prevDone != nil {
		select {
		case <-state.prevDone:
		case <-ctx.Done():
			log.Printf("cancelled waiting for previous save on %s: %v", trackingKey, ctx.Err())
			b.metaMu.Lock()
			b.removePendingSaveLocked(trackingKey, state)
			b.metaMu.Unlock()
			return
		}
	}

	if b.beforePersistSave != nil {
		b.beforePersistSave()
	}

	b.metaMu.Lock()
	if state.deleted || state.superseded {
		b.removePendingSaveLocked(trackingKey, state)
		wasDeletedBefore := state.deleted
		targetDeleteID := trackingKey
		if isPendingID(targetDeleteID) && pendingID != "" {
			if realID, ok := b.idAliases[pendingID]; ok && !isPendingID(realID) {
				targetDeleteID = realID
			}
		}
		b.metaMu.Unlock()
		if wasDeletedBefore && !isPendingID(targetDeleteID) {
			func() {
				delCtx, delCancel := context.WithTimeout(context.Background(), b.saveTimeout())
				defer delCancel()
				if delErr := b.raw.Delete(delCtx, targetDeleteID); delErr != nil && !errors.Is(delErr, ErrNotFound) {
					log.Printf("failed to delete secret %s after cancelled in-flight save: %v", targetDeleteID, delErr)
				}
			}()
		}
		return
	}
	// If an earlier save resolved the pending ID to a real ID, reuse it so we update instead of creating a duplicate item.
	if item.ID == "" && pendingID != "" {
		if realID, ok := b.idAliases[pendingID]; ok && realID != "" {
			item.ID = realID
		}
	}
	b.metaMu.Unlock()

	err := b.raw.Save(ctx, item)

	b.metaMu.Lock()
	wasDeleted := state.deleted
	wasSuperseded := state.superseded
	b.removePendingSaveLocked(trackingKey, state)
	if !wasDeleted && err == nil && pendingID != "" && item.ID != "" && item.ID != pendingID {
		b.idAliases[pendingID] = item.ID

		// Migrate queued pending saves for pendingID to real item.ID to preserve serialization.
		if remaining := b.pendingSaves[pendingID]; len(remaining) > 0 {
			b.pendingSaves[item.ID] = append(b.pendingSaves[item.ID], remaining...)
			delete(b.pendingSaves, pendingID)
		}

		// Always rename cache keys to the real item ID so cache hits and metadata match.
		if cached := b.metaCache[pendingID]; cached != nil {
			delete(b.metaCache, pendingID)
			cached.ID = item.ID
			b.metaCache[item.ID] = cached
		} else if b.opts.CacheMetadata {
			copied := copySecretItem(item)
			copied.Secret = nil
			b.metaCache[item.ID] = copied
		}

		b.secretMu.Lock()
		if entry := b.secretCache[pendingID]; entry != nil {
			delete(b.secretCache, pendingID)
			entry.id = item.ID
			if old := b.secretCache[item.ID]; old != nil && old != entry {
				old.destroy()
			}
			b.secretCache[item.ID] = entry
		}
		b.secretMu.Unlock()
	}

	targetDeleteID := trackingKey
	if wasDeleted {
		if !state.isNew {
			// existing item
		} else if err == nil && item.ID != "" && !isPendingID(item.ID) {
			targetDeleteID = item.ID
		} else if pendingID != "" {
			if realID, ok := b.idAliases[pendingID]; ok && !isPendingID(realID) {
				targetDeleteID = realID
			}
		}
	}
	b.metaMu.Unlock()

	if wasDeleted {
		if !isPendingID(targetDeleteID) {
			func() {
				delCtx, delCancel := context.WithTimeout(context.Background(), b.saveTimeout())
				defer delCancel()
				if delErr := b.raw.Delete(delCtx, targetDeleteID); delErr != nil && !errors.Is(delErr, ErrNotFound) {
					log.Printf("failed to delete secret %s after cancelled in-flight save: %v", targetDeleteID, delErr)
				}
			}()
		}
		return
	}

	if wasSuperseded {
		return
	}

	if err != nil {
		log.Printf("failed to persist secret: %v", err)
		return
	}
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
