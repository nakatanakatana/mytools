package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeRawBackend struct {
	mu             sync.Mutex
	items          map[string]*SecretItem
	getCalls       int
	loadMetaCalls  int
	saveStarted    chan struct{}
	saveRelease    chan struct{}
	saveErr        error
	createdRealID  string
	failAuthCheck  bool
	authCheckCalls int
}

func newFakeRawBackend() *fakeRawBackend {
	return &fakeRawBackend{
		items: make(map[string]*SecretItem),
	}
}

func (b *fakeRawBackend) Get(ctx context.Context, id string) (*SecretItem, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.getCalls++
	item := b.items[id]
	if item == nil {
		return nil, ErrNotFound
	}
	return copySecretItem(item), nil
}

func (b *fakeRawBackend) Save(ctx context.Context, item *SecretItem) error {
	if b.saveStarted != nil {
		close(b.saveStarted)
	}
	if b.saveRelease != nil {
		<-b.saveRelease
	}
	if b.saveErr != nil {
		return b.saveErr
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	copied := copySecretItem(item)
	if copied.ID == "" {
		copied.ID = b.createdRealID
		item.ID = copied.ID
	}
	b.items[copied.ID] = copied
	return nil
}

func (b *fakeRawBackend) Delete(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.items[id] == nil {
		return ErrNotFound
	}
	delete(b.items, id)
	return nil
}

func (b *fakeRawBackend) LoadMetadata(ctx context.Context) ([]*SecretItem, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loadMetaCalls++
	items := make([]*SecretItem, 0, len(b.items))
	for _, item := range b.items {
		copied := copySecretItem(item)
		copied.Secret = nil
		items = append(items, copied)
	}
	return items, nil
}

func (b *fakeRawBackend) CheckAuth(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.authCheckCalls++
	if b.failAuthCheck {
		return fmt.Errorf("not signed in")
	}
	return nil
}

func TestCachedBackend_Get_UsesSecretCacheWithSlidingTTL(t *testing.T) {
	now := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	raw := newFakeRawBackend()
	raw.items["id1"] = &SecretItem{ID: "id1", Label: "label", Attributes: map[string]string{"service": "github"}, Secret: []byte("token-1")}
	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:        true,
		SecretCacheTTL:      time.Minute,
		AuthCheckMinSpacing: time.Hour,
		Now: func() time.Time {
			return now
		},
	})

	first, err := backend.Get(context.Background(), "id1")
	if err != nil {
		t.Fatalf("first Get failed: %v", err)
	}
	if string(first.Secret) != "token-1" {
		t.Fatalf("first secret = %q", first.Secret)
	}

	raw.mu.Lock()
	raw.items["id1"].Secret = []byte("token-2")
	raw.mu.Unlock()

	now = now.Add(30 * time.Second)
	second, err := backend.Get(context.Background(), "id1")
	if err != nil {
		t.Fatalf("second Get failed: %v", err)
	}
	if string(second.Secret) != "token-1" {
		t.Fatalf("second secret = %q", second.Secret)
	}

	now = now.Add(61 * time.Second)
	third, err := backend.Get(context.Background(), "id1")
	if err != nil {
		t.Fatalf("third Get failed: %v", err)
	}
	if string(third.Secret) != "token-2" {
		t.Fatalf("third secret = %q", third.Secret)
	}
	if raw.getCalls != 2 {
		t.Fatalf("raw Get calls = %d, want 2", raw.getCalls)
	}
}

func TestCachedBackend_Search_UsesMetadataCache(t *testing.T) {
	raw := newFakeRawBackend()
	raw.items["id1"] = &SecretItem{ID: "id1", Label: "one", Attributes: map[string]string{"service": "github", "username": "alice"}, Secret: []byte("secret")}
	backend := NewCachedBackend(raw, BackendOptions{CacheMetadata: true})

	first, err := backend.Search(context.Background(), map[string]string{"service": "github"})
	if err != nil {
		t.Fatalf("first Search failed: %v", err)
	}
	if len(first) != 1 || first[0].ID != "id1" || first[0].Secret != nil {
		t.Fatalf("unexpected first Search result: %+v", first)
	}

	raw.mu.Lock()
	raw.items["id2"] = &SecretItem{ID: "id2", Label: "two", Attributes: map[string]string{"service": "github"}, Secret: []byte("new")}
	raw.mu.Unlock()

	second, err := backend.Search(context.Background(), map[string]string{"service": "github"})
	if err != nil {
		t.Fatalf("second Search failed: %v", err)
	}
	if len(second) != 1 || second[0].ID != "id1" {
		t.Fatalf("metadata cache was not used: %+v", second)
	}
	if raw.loadMetaCalls != 1 {
		t.Fatalf("LoadMetadata calls = %d, want 1", raw.loadMetaCalls)
	}
}

func TestCachedBackend_Save_AsyncUpdatesCacheBeforePersistenceCompletes(t *testing.T) {
	raw := newFakeRawBackend()
	raw.saveStarted = make(chan struct{})
	raw.saveRelease = make(chan struct{})
	raw.createdRealID = "real-id"
	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:        true,
		CacheMetadata:       true,
		AsyncSave:           true,
		SecretCacheTTL:      time.Minute,
		AuthCheckMinSpacing: time.Hour,
	})

	item := &SecretItem{
		Label:      "label",
		Attributes: map[string]string{"service": "github"},
		Secret:     []byte("secret"),
	}
	if err := backend.Save(context.Background(), item); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if item.ID == "" {
		t.Fatal("Save did not assign pending ID")
	}

	select {
	case <-raw.saveStarted:
	case <-time.After(time.Second):
		t.Fatal("raw Save did not start")
	}

	got, err := backend.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("Get pending item failed: %v", err)
	}
	if string(got.Secret) != "secret" {
		t.Fatalf("cached secret = %q", got.Secret)
	}

	close(raw.saveRelease)
	deadline := time.After(time.Second)
	for {
		matches, err := backend.Search(context.Background(), map[string]string{"service": "github"})
		if err == nil && len(matches) == 1 && matches[0].ID == "real-id" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for pending ID reconciliation")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestCachedBackend_Delete_InvalidatesSecretCache(t *testing.T) {
	raw := newFakeRawBackend()
	raw.items["id1"] = &SecretItem{ID: "id1", Label: "label", Attributes: map[string]string{"service": "github"}, Secret: []byte("token-1")}
	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:        true,
		SecretCacheTTL:      time.Minute,
		AuthCheckMinSpacing: time.Hour,
	})

	if _, err := backend.Get(context.Background(), "id1"); err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	raw.mu.Lock()
	raw.items["id1"].Secret = []byte("token-2")
	raw.mu.Unlock()

	if err := backend.Delete(context.Background(), "id1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	raw.mu.Lock()
	raw.items["id1"] = &SecretItem{ID: "id1", Label: "label", Attributes: map[string]string{"service": "github"}, Secret: []byte("token-3")}
	raw.mu.Unlock()

	got, err := backend.Get(context.Background(), "id1")
	if err != nil {
		t.Fatalf("Get after Delete failed: %v", err)
	}
	if string(got.Secret) != "token-3" {
		t.Fatalf("secret after Delete = %q", got.Secret)
	}
	if raw.getCalls != 2 {
		t.Fatalf("raw Get calls = %d, want 2", raw.getCalls)
	}
}

func TestCachedBackend_Get_AuthCheckFailureClearsSecretCache(t *testing.T) {
	raw := newFakeRawBackend()
	raw.items["id1"] = &SecretItem{ID: "id1", Label: "label", Attributes: map[string]string{"service": "github"}, Secret: []byte("token-1")}
	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:        true,
		SecretCacheTTL:      time.Minute,
		AuthCheckMinSpacing: time.Nanosecond,
	})

	if _, err := backend.Get(context.Background(), "id1"); err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	raw.mu.Lock()
	raw.items["id1"].Secret = []byte("token-2")
	raw.failAuthCheck = true
	raw.mu.Unlock()

	second, err := backend.Get(context.Background(), "id1")
	if err != nil {
		t.Fatalf("cached Get failed: %v", err)
	}
	if string(second.Secret) != "token-1" {
		t.Fatalf("cached secret = %q", second.Secret)
	}

	deadline := time.After(time.Second)
	for {
		raw.mu.Lock()
		authCheckCalls := raw.authCheckCalls
		raw.mu.Unlock()
		if authCheckCalls > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for async auth check")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	third, err := backend.Get(context.Background(), "id1")
	if err != nil {
		t.Fatalf("Get after auth failure failed: %v", err)
	}
	if string(third.Secret) != "token-2" {
		t.Fatalf("secret after auth failure = %q", third.Secret)
	}
	if raw.getCalls != 2 {
		t.Fatalf("raw Get calls = %d, want 2", raw.getCalls)
	}
}

func TestCachedBackend_List_ReturnsMetadataWithoutSecrets(t *testing.T) {
	raw := newFakeRawBackend()
	raw.items["id1"] = &SecretItem{ID: "id1", Label: "label", Attributes: map[string]string{"service": "github"}, Secret: []byte("secret")}
	backend := NewCachedBackend(raw, BackendOptions{CacheMetadata: true})

	items, err := backend.List(context.Background())
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List returned %d items, want 1", len(items))
	}
	if items[0].ID != "id1" || items[0].Secret != nil {
		t.Fatalf("unexpected List item: %+v", items[0])
	}
}

func TestCachedBackend_CanDisableSecretAndMetadataCaches(t *testing.T) {
	raw := newFakeRawBackend()
	raw.items["id1"] = &SecretItem{ID: "id1", Label: "label", Attributes: map[string]string{"service": "github"}, Secret: []byte("token-1")}
	backend := NewCachedBackend(raw, BackendOptions{})

	first, err := backend.Get(context.Background(), "id1")
	if err != nil {
		t.Fatalf("first Get failed: %v", err)
	}
	if string(first.Secret) != "token-1" {
		t.Fatalf("first secret = %q", first.Secret)
	}

	raw.mu.Lock()
	raw.items["id1"].Secret = []byte("token-2")
	raw.mu.Unlock()

	second, err := backend.Get(context.Background(), "id1")
	if err != nil {
		t.Fatalf("second Get failed: %v", err)
	}
	if string(second.Secret) != "token-2" {
		t.Fatalf("second secret = %q", second.Secret)
	}

	if _, err := backend.Search(context.Background(), map[string]string{"service": "github"}); err != nil {
		t.Fatalf("first Search failed: %v", err)
	}
	if _, err := backend.Search(context.Background(), map[string]string{"service": "github"}); err != nil {
		t.Fatalf("second Search failed: %v", err)
	}
	if raw.getCalls != 2 {
		t.Fatalf("raw Get calls = %d, want 2", raw.getCalls)
	}
	if raw.loadMetaCalls != 2 {
		t.Fatalf("LoadMetadata calls = %d, want 2", raw.loadMetaCalls)
	}
}

func TestCachedBackend_AsyncSaveFailureKeepsOptimisticCache(t *testing.T) {
	raw := newFakeRawBackend()
	raw.saveErr = fmt.Errorf("persist failed")
	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:        true,
		CacheMetadata:       true,
		AsyncSave:           true,
		SecretCacheTTL:      time.Minute,
		AuthCheckMinSpacing: time.Hour,
	})

	item := &SecretItem{
		ID:         "id1",
		Label:      "label",
		Attributes: map[string]string{"service": "github"},
		Secret:     []byte("secret"),
	}
	if err := backend.Save(context.Background(), item); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	got, err := backend.Get(context.Background(), "id1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if string(got.Secret) != "secret" {
		t.Fatalf("cached secret = %q", got.Secret)
	}
}
