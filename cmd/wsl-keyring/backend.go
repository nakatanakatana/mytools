package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrNotFound = errors.New("secret not found")
)

// SecretItem represents a secret stored in the keyring.
type SecretItem struct {
	ID         string            `json:"id"`
	Label      string            `json:"label"`
	Attributes map[string]string `json:"attributes"`
	Secret     []byte            `json:"secret"`
}

// StorageBackend is the interface that wraps basic storage operations.
type StorageBackend interface {
	Search(ctx context.Context, attributes map[string]string) ([]*SecretItem, error)
	Get(ctx context.Context, id string) (*SecretItem, error)
	Save(ctx context.Context, item *SecretItem) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]*SecretItem, error)
}

// RawStorageBackend provides backend-specific persistence primitives.
// Shared StorageBackend behavior such as Search/List filtering and caching is
// layered on top by CachedBackend.
type RawStorageBackend interface {
	Get(ctx context.Context, id string) (*SecretItem, error)
	Save(ctx context.Context, item *SecretItem) error
	Delete(ctx context.Context, id string) error
	LoadMetadata(ctx context.Context) ([]*SecretItem, error)
}

// AuthChecker can be implemented by slow backends whose cached secrets should
// be discarded when their external authentication state expires.
type AuthChecker interface {
	CheckAuth(ctx context.Context) error
}

// InMemoryBackend implements StorageBackend using an in-memory map.
type InMemoryBackend struct {
	mu    sync.RWMutex
	items map[string]*SecretItem
}

func NewInMemoryBackend() *InMemoryBackend {
	return &InMemoryBackend{
		items: make(map[string]*SecretItem),
	}
}

func (b *InMemoryBackend) Search(ctx context.Context, attributes map[string]string) ([]*SecretItem, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var matched []*SecretItem
	for _, item := range b.items {
		match := true
		for k, v := range attributes {
			val, ok := item.Attributes[k]
			if !ok || val != v {
				match = false
				break
			}
		}
		if match {
			matched = append(matched, item)
		}
	}
	return matched, nil
}

func (b *InMemoryBackend) Get(ctx context.Context, id string) (*SecretItem, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	item, ok := b.items[id]
	if !ok {
		return nil, ErrNotFound
	}
	return item, nil
}

func (b *InMemoryBackend) Save(ctx context.Context, item *SecretItem) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if item.ID == "" {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			return fmt.Errorf("failed to generate item ID: %w", err)
		}
		item.ID = fmt.Sprintf("%x", buf)
	}
	b.items[item.ID] = item
	return nil
}

func (b *InMemoryBackend) Delete(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.items[id]; !ok {
		return ErrNotFound
	}
	delete(b.items, id)
	return nil
}

func (b *InMemoryBackend) List(ctx context.Context) ([]*SecretItem, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	list := make([]*SecretItem, 0, len(b.items))
	for _, item := range b.items {
		list = append(list, item)
	}
	return list, nil
}

func (b *InMemoryBackend) LoadMetadata(ctx context.Context) ([]*SecretItem, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	list := make([]*SecretItem, 0, len(b.items))
	for _, item := range b.items {
		copied := copySecretItem(item)
		copied.Secret = nil
		list = append(list, copied)
	}
	return list, nil
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
