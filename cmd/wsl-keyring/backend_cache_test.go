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
	loadMetaDelay  time.Duration
	saveStarted    chan struct{}
	saveRelease    chan struct{}
	saveErr        error
	createdRealID  string
	createdRealIDs []string
	createdIDSeq   int
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
		select {
		case <-b.saveStarted:
		default:
			close(b.saveStarted)
		}
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
		if len(b.createdRealIDs) > 0 {
			copied.ID = b.createdRealIDs[0]
			b.createdRealIDs = b.createdRealIDs[1:]
		} else if b.createdRealID != "" {
			copied.ID = b.createdRealID
		} else {
			b.createdIDSeq++
			copied.ID = fmt.Sprintf("real-uuid-%d", b.createdIDSeq)
		}
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
	if b.loadMetaDelay > 0 {
		time.Sleep(b.loadMetaDelay)
	}
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

func TestCachedBackend_Search_CoalescesConcurrentMetadataLoads(t *testing.T) {
	raw := newFakeRawBackend()
	raw.loadMetaDelay = 50 * time.Millisecond
	raw.items["id1"] = &SecretItem{ID: "id1", Label: "one", Attributes: map[string]string{"service": "github"}, Secret: []byte("secret")}
	backend := NewCachedBackend(raw, BackendOptions{CacheMetadata: true})

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			matches, err := backend.Search(context.Background(), map[string]string{"service": "github"})
			if err != nil {
				errs <- err
				return
			}
			if len(matches) != 1 || matches[0].ID != "id1" {
				errs <- fmt.Errorf("unexpected matches: %+v", matches)
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	raw.mu.Lock()
	defer raw.mu.Unlock()
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

func TestCachedBackend_Delete_WhileAsyncSaveInFlight_RemovesPersistedItem(t *testing.T) {
	raw := newFakeRawBackend()
	raw.createdRealID = "real-uuid-1234"
	raw.saveStarted = make(chan struct{})
	raw.saveRelease = make(chan struct{})

	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
	})

	item := &SecretItem{
		Label:      "Password for '' on 'glab:__keyring_probe__:123:1'",
		Attributes: map[string]string{"service": "glab:__keyring_probe__:123:1", "username": ""},
		Secret:     []byte("1"),
	}

	if err := backend.Save(context.Background(), item); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	pendingID := item.ID
	if pendingID == "" {
		t.Fatal("expected pending ID to be assigned")
	}

	<-raw.saveStarted

	// Call Delete while raw.Save is still in flight
	if err := backend.Delete(context.Background(), pendingID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	// Verify that the item is immediately removed from cache
	if _, err := backend.Get(context.Background(), pendingID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound from cache, got: %v", err)
	}

	// Release the in-flight Save
	close(raw.saveRelease)

	// Wait briefly for persistSave to complete and clean up
	var rawItem *SecretItem
	for i := 0; i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		raw.mu.Lock()
		rawItem = raw.items["real-uuid-1234"]
		raw.mu.Unlock()
		if rawItem == nil {
			break
		}
	}

	if rawItem != nil {
		t.Fatalf("item was persisted in raw backend despite Delete: %+v", rawItem)
	}
}

func TestCachedBackend_Delete_BeforeAsyncSaveStarts_SkipsSave(t *testing.T) {
	raw := newFakeRawBackend()
	raw.createdRealID = "real-uuid-5678"

	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
	})

	persistStarted := make(chan struct{})
	persistRelease := make(chan struct{})
	backend.beforePersistSave = func() {
		close(persistStarted)
		<-persistRelease
	}

	item := &SecretItem{
		Label:      "probe item",
		Attributes: map[string]string{"service": "glab:__keyring_probe__:999:1"},
		Secret:     []byte("test"),
	}

	if err := backend.Save(context.Background(), item); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	pendingID := item.ID

	// Wait until persistSave goroutine enters beforePersistSave, holding before the deleted check
	<-persistStarted

	// Delete while persistSave is blocked before deleted check
	if err := backend.Delete(context.Background(), pendingID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	// Verify that the item is not in cache
	if _, err := backend.Get(context.Background(), pendingID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound from cache, got: %v", err)
	}

	// Let persistSave proceed
	close(persistRelease)

	// Poll briefly for cleanup
	for i := 0; i < 50; i++ {
		backend.metaMu.Lock()
		pendingLen := len(backend.pendingSaves)
		backend.metaMu.Unlock()
		if pendingLen == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	raw.mu.Lock()
	rawSaveCalls := len(raw.items)
	raw.mu.Unlock()

	if rawSaveCalls != 0 {
		t.Fatalf("expected raw items to be empty, got: %d", rawSaveCalls)
	}
}

func TestCachedBackend_Delete_ExistingItem_BeforeAsyncSaveStarts(t *testing.T) {
	raw := newFakeRawBackend()
	existingItem := &SecretItem{
		ID:         "real-uuid-existing-1",
		Label:      "existing item",
		Attributes: map[string]string{"service": "github"},
		Secret:     []byte("old-secret"),
	}
	raw.items[existingItem.ID] = copySecretItem(existingItem)

	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
	})

	persistStarted := make(chan struct{})
	persistRelease := make(chan struct{})
	backend.beforePersistSave = func() {
		close(persistStarted)
		<-persistRelease
	}

	// Update existing item
	updateItem := copySecretItem(existingItem)
	updateItem.Secret = []byte("new-secret")
	if err := backend.Save(context.Background(), updateItem); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	<-persistStarted

	// Delete existing item before persistSave proceeds
	if err := backend.Delete(context.Background(), existingItem.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	// Release persistSave
	close(persistRelease)

	// Poll for raw backend to reflect deletion
	var rawItem *SecretItem
	for i := 0; i < 50; i++ {
		raw.mu.Lock()
		rawItem = raw.items[existingItem.ID]
		raw.mu.Unlock()
		if rawItem == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if rawItem != nil {
		t.Fatalf("existing item was not deleted from raw backend: %+v", rawItem)
	}
}

func TestCachedBackend_Delete_ExistingItem_WhileAsyncSaveInFlight(t *testing.T) {
	raw := newFakeRawBackend()
	existingItem := &SecretItem{
		ID:         "real-uuid-existing-2",
		Label:      "existing item 2",
		Attributes: map[string]string{"service": "gitlab"},
		Secret:     []byte("old-secret"),
	}
	raw.items[existingItem.ID] = copySecretItem(existingItem)

	raw.saveStarted = make(chan struct{})
	raw.saveRelease = make(chan struct{})

	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
	})

	// Update existing item
	updateItem := copySecretItem(existingItem)
	updateItem.Secret = []byte("new-secret")
	if err := backend.Save(context.Background(), updateItem); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	<-raw.saveStarted

	// Delete while raw.Save is running
	if err := backend.Delete(context.Background(), existingItem.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	// Release raw.Save
	close(raw.saveRelease)

	// Poll for raw backend to reflect deletion
	var rawItem *SecretItem
	for i := 0; i < 50; i++ {
		raw.mu.Lock()
		rawItem = raw.items[existingItem.ID]
		raw.mu.Unlock()
		if rawItem == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if rawItem != nil {
		t.Fatalf("existing item was not deleted from raw backend: %+v", rawItem)
	}
}

func TestCachedBackend_Delete_ExistingItem_WhileAsyncSaveInFlight_SaveFails(t *testing.T) {
	raw := newFakeRawBackend()
	existingItem := &SecretItem{
		ID:         "real-uuid-existing-3",
		Label:      "existing item 3",
		Attributes: map[string]string{"service": "bitbucket"},
		Secret:     []byte("old-secret"),
	}
	raw.items[existingItem.ID] = copySecretItem(existingItem)

	raw.saveStarted = make(chan struct{})
	raw.saveRelease = make(chan struct{})
	raw.saveErr = fmt.Errorf("network error during save")

	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
	})

	// Update existing item
	updateItem := copySecretItem(existingItem)
	updateItem.Secret = []byte("new-secret")
	if err := backend.Save(context.Background(), updateItem); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	<-raw.saveStarted

	// Delete while raw.Save is running
	if err := backend.Delete(context.Background(), existingItem.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	// Release raw.Save
	close(raw.saveRelease)

	// Poll for raw backend to reflect deletion
	var rawItem *SecretItem
	for i := 0; i < 50; i++ {
		raw.mu.Lock()
		rawItem = raw.items[existingItem.ID]
		raw.mu.Unlock()
		if rawItem == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if rawItem != nil {
		t.Fatalf("existing item was not deleted from raw backend after save failure: %+v", rawItem)
	}
}

func TestCachedBackend_Delete_AfterMultipleConcurrentSaves_DoesNotResurrect(t *testing.T) {
	raw := newFakeRawBackend()
	existingItem := &SecretItem{
		ID:         "real-uuid-concurrent-1",
		Label:      "concurrent item",
		Attributes: map[string]string{"service": "github"},
		Secret:     []byte("secret-v1"),
	}
	raw.items[existingItem.ID] = copySecretItem(existingItem)

	raw.saveStarted = make(chan struct{})
	raw.saveRelease = make(chan struct{})

	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
	})

	// Save1: in flight and blocked in raw.Save
	item1 := copySecretItem(existingItem)
	item1.Secret = []byte("secret-v2")
	if err := backend.Save(context.Background(), item1); err != nil {
		t.Fatalf("Save1 failed: %v", err)
	}
	<-raw.saveStarted

	// Save2: issued while Save1 is still running
	item2 := copySecretItem(existingItem)
	item2.Secret = []byte("secret-v3")
	if err := backend.Save(context.Background(), item2); err != nil {
		t.Fatalf("Save2 failed: %v", err)
	}

	// Delete: issued while both or one save is in flight
	if err := backend.Delete(context.Background(), existingItem.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Release raw.Save so Save1 completes
	close(raw.saveRelease)

	// Poll to ensure the item is not resurrected and remains deleted
	var rawItem *SecretItem
	for i := 0; i < 50; i++ {
		raw.mu.Lock()
		rawItem = raw.items[existingItem.ID]
		raw.mu.Unlock()
		if rawItem == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if rawItem != nil {
		t.Fatalf("item was resurrected in raw backend: %+v", rawItem)
	}
}

func TestCachedBackend_Save_WithUnresolvedPendingID_TreatedAsNew(t *testing.T) {
	raw := newFakeRawBackend()
	raw.createdRealID = "real-uuid-from-pending"
	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
	})

	// Pass an item with a pending_ prefix as its ID
	item := &SecretItem{
		ID:         "pending_test_12345",
		Label:      "item with pending ID",
		Attributes: map[string]string{"service": "test"},
		Secret:     []byte("secret"),
	}
	if err := backend.Save(context.Background(), item); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Poll to wait for async save
	for i := 0; i < 50; i++ {
		backend.metaMu.Lock()
		pendingLen := len(backend.pendingSaves)
		backend.metaMu.Unlock()
		if pendingLen == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	raw.mu.Lock()
	savedItem := raw.items["real-uuid-from-pending"]
	raw.mu.Unlock()

	if savedItem == nil {
		t.Fatal("expected item to be created with real ID, but not found in raw backend")
	}
}

func TestCachedBackend_Delete_UnresolvedPendingID_IsIdempotent(t *testing.T) {
	raw := newFakeRawBackend()
	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
	})

	// Deleting a pending_ ID that does not exist in pendingSaves or raw backend should not error
	if err := backend.Delete(context.Background(), "pending_non_existent"); err != nil {
		t.Fatalf("Delete returned unexpected error for non-existent pending ID: %v", err)
	}
}

func TestCachedBackend_Save_SupersededNewItem_ReusesCreatedRealID(t *testing.T) {
	raw := newFakeRawBackend()
	raw.createdRealIDs = []string{"real-uuid-1"}
	raw.saveStarted = make(chan struct{})
	raw.saveRelease = make(chan struct{})

	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
	})

	// Save1: new item, blocked in raw.Save
	item1 := &SecretItem{
		Label:      "probe item 1",
		Attributes: map[string]string{"service": "test:superseded"},
		Secret:     []byte("secret-v1"),
	}
	if err := backend.Save(context.Background(), item1); err != nil {
		t.Fatalf("Save1 failed: %v", err)
	}
	<-raw.saveStarted

	// Save2: update on the same pending ID while Save1 is still blocked in raw.Save
	item2 := copySecretItem(item1)
	item2.Secret = []byte("secret-v2")
	if err := backend.Save(context.Background(), item2); err != nil {
		t.Fatalf("Save2 failed: %v", err)
	}

	// Release Save1
	close(raw.saveRelease)

	// Wait for background saves to settle
	for i := 0; i < 50; i++ {
		backend.metaMu.Lock()
		pendingLen := len(backend.pendingSaves)
		backend.metaMu.Unlock()
		if pendingLen == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Check raw backend: exactly 1 item should exist with real-uuid-1 and secret-v2
	raw.mu.Lock()
	itemCount := len(raw.items)
	realItem := raw.items["real-uuid-1"]
	raw.mu.Unlock()

	if itemCount != 1 {
		t.Fatalf("expected exactly 1 item in raw backend, got %d", itemCount)
	}
	if realItem == nil {
		t.Fatal("expected real-uuid-1 to exist in raw backend")
	}
	if string(realItem.Secret) != "secret-v2" {
		t.Fatalf("expected raw secret to be updated to secret-v2, got %q", string(realItem.Secret))
	}

	// Verify cached secret is also secret-v2
	got, err := backend.Get(context.Background(), item1.ID)
	if err != nil {
		t.Fatalf("failed to get item from cache: %v", err)
	}
	if string(got.Secret) != "secret-v2" {
		t.Fatalf("expected cached secret to be secret-v2, got %q", string(got.Secret))
	}
}

func TestCachedBackend_Delete_RealID_CancelsPendingSavesAndDeletesItem(t *testing.T) {
	raw := newFakeRawBackend()
	raw.createdRealIDs = []string{"real-uuid-1"}
	raw.saveStarted = make(chan struct{})
	raw.saveRelease = make(chan struct{})

	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
	})

	// Save1: new item, blocked in raw.Save
	item1 := &SecretItem{
		Label:      "probe item 1",
		Attributes: map[string]string{"service": "test:delete_real"},
		Secret:     []byte("secret-v1"),
	}
	if err := backend.Save(context.Background(), item1); err != nil {
		t.Fatalf("Save1 failed: %v", err)
	}
	<-raw.saveStarted

	// Save2: update on the same pending ID while Save1 is blocked in raw.Save
	item2 := copySecretItem(item1)
	item2.Secret = []byte("secret-v2")
	if err := backend.Save(context.Background(), item2); err != nil {
		t.Fatalf("Save2 failed: %v", err)
	}

	// Release Save1 so it finishes and registers real-uuid-1 in idAliases
	close(raw.saveRelease)

	// Wait until real-uuid-1 is registered in idAliases
	for i := 0; i < 50; i++ {
		backend.metaMu.Lock()
		alias := backend.idAliases[item1.ID]
		backend.metaMu.Unlock()
		if alias == "real-uuid-1" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Now Delete using the real ID "real-uuid-1" while Save2 may be in flight or queued
	if err := backend.Delete(context.Background(), "real-uuid-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Wait for all pending saves to settle
	for i := 0; i < 50; i++ {
		backend.metaMu.Lock()
		pendingLen := len(backend.pendingSaves)
		backend.metaMu.Unlock()
		if pendingLen == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Check raw backend: real-uuid-1 must NOT remain
	raw.mu.Lock()
	itemCount := len(raw.items)
	raw.mu.Unlock()

	if itemCount != 0 {
		t.Fatalf("expected 0 items in raw backend after Delete, got %d", itemCount)
	}
}

func TestCachedBackend_Delete_PendingIDAfterSave1Done_CleansUpRealItem(t *testing.T) {
	raw := newFakeRawBackend()
	raw.createdRealIDs = []string{"real-uuid-1"}
	raw.saveStarted = make(chan struct{})
	raw.saveRelease = make(chan struct{})

	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
	})

	// Save1: new item, blocked in raw.Save
	item1 := &SecretItem{
		Label:      "probe item 1",
		Attributes: map[string]string{"service": "test:delete_pending"},
		Secret:     []byte("secret-v1"),
	}
	if err := backend.Save(context.Background(), item1); err != nil {
		t.Fatalf("Save1 failed: %v", err)
	}
	<-raw.saveStarted

	// Save2: update on the same pending ID while Save1 is blocked in raw.Save
	item2 := copySecretItem(item1)
	item2.Secret = []byte("secret-v2")
	if err := backend.Save(context.Background(), item2); err != nil {
		t.Fatalf("Save2 failed: %v", err)
	}

	// Release Save1 so it finishes and registers real-uuid-1 in idAliases
	close(raw.saveRelease)

	// Wait until real-uuid-1 is registered in idAliases
	for i := 0; i < 50; i++ {
		backend.metaMu.Lock()
		alias := backend.idAliases[item1.ID]
		backend.metaMu.Unlock()
		if alias == "real-uuid-1" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Delete using the original pending ID (item1.ID)
	if err := backend.Delete(context.Background(), item1.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Wait for all pending saves to settle
	for i := 0; i < 50; i++ {
		backend.metaMu.Lock()
		pendingLen := len(backend.pendingSaves)
		backend.metaMu.Unlock()
		if pendingLen == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Check raw backend: real-uuid-1 must NOT remain
	raw.mu.Lock()
	itemCount := len(raw.items)
	raw.mu.Unlock()

	if itemCount != 0 {
		t.Fatalf("expected 0 items in raw backend after Delete, got %d", itemCount)
	}
}

func TestCachedBackend_Save_TimeoutWaitingForPrevDone_CleansPendingSaves(t *testing.T) {
	raw := newFakeRawBackend()
	raw.createdRealIDs = []string{"real-uuid-timeout"}
	raw.saveStarted = make(chan struct{})
	raw.saveRelease = make(chan struct{})

	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
		SaveTimeout:   50 * time.Millisecond,
	})

	item1 := &SecretItem{
		Label:      "probe item 1",
		Attributes: map[string]string{"service": "test:timeout_prev_done"},
		Secret:     []byte("secret-v1"),
	}
	if err := backend.Save(context.Background(), item1); err != nil {
		t.Fatalf("Save1 failed: %v", err)
	}
	<-raw.saveStarted

	// Save2 queues behind Save1, but with SaveTimeout=50ms, it will time out waiting for Save1's done channel
	item2 := copySecretItem(item1)
	item2.Secret = []byte("secret-v2")
	if err := backend.Save(context.Background(), item2); err != nil {
		t.Fatalf("Save2 failed: %v", err)
	}

	// Wait enough time for Save2's context to expire
	time.Sleep(150 * time.Millisecond)

	// Save1 is still blocked; Save2 should have timed out and removed its state from pendingSaves
	backend.metaMu.Lock()
	queuedCount := len(backend.pendingSaves[item1.ID])
	backend.metaMu.Unlock()
	if queuedCount != 1 {
		t.Fatalf("expected 1 remaining save (Save1 only) after Save2 timeout, got %d", queuedCount)
	}

	// Release Save1 and let it finish
	close(raw.saveRelease)

	// Verify all pending saves eventually clear
	for i := 0; i < 50; i++ {
		backend.metaMu.Lock()
		pendingLen := len(backend.pendingSaves)
		backend.metaMu.Unlock()
		if pendingLen == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	backend.metaMu.Lock()
	finalPendingLen := len(backend.pendingSaves)
	backend.metaMu.Unlock()
	if finalPendingLen != 0 {
		t.Fatalf("expected 0 pending saves, got %d", finalPendingLen)
	}
}

func TestCachedBackend_Delete_ConcurrentWithQueuedSave_CleansPendingSavesViaFallback(t *testing.T) {
	raw := newFakeRawBackend()
	raw.createdRealIDs = []string{"real-uuid-fallback"}
	raw.saveStarted = make(chan struct{})
	raw.saveRelease = make(chan struct{})

	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
	})

	item1 := &SecretItem{
		Label:      "probe item 1",
		Attributes: map[string]string{"service": "test:fallback_clean"},
		Secret:     []byte("secret-v1"),
	}
	if err := backend.Save(context.Background(), item1); err != nil {
		t.Fatalf("Save1 failed: %v", err)
	}
	<-raw.saveStarted

	// Save2 queued behind Save1
	item2 := copySecretItem(item1)
	item2.Secret = []byte("secret-v2")
	if err := backend.Save(context.Background(), item2); err != nil {
		t.Fatalf("Save2 failed: %v", err)
	}

	// Release Save1 so it creates real-uuid-fallback, migrates pendingSaves to "real-uuid-fallback", and registers alias
	close(raw.saveRelease)

	// Wait until real-uuid-fallback is registered in idAliases
	for i := 0; i < 50; i++ {
		backend.metaMu.Lock()
		alias := backend.idAliases[item1.ID]
		backend.metaMu.Unlock()
		if alias == "real-uuid-fallback" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Call Delete with real-uuid-fallback. This clears idAliases for both "real-uuid-fallback" and item1.ID
	if err := backend.Delete(context.Background(), "real-uuid-fallback"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Wait for all pending saves to complete and clear
	for i := 0; i < 50; i++ {
		backend.metaMu.Lock()
		pendingLen := len(backend.pendingSaves)
		backend.metaMu.Unlock()
		if pendingLen == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	backend.metaMu.Lock()
	finalPendingLen := len(backend.pendingSaves)
	backend.metaMu.Unlock()
	if finalPendingLen != 0 {
		t.Fatalf("expected 0 pending saves after Delete and Save2 cleanup, got %d", finalPendingLen)
	}
}

func TestCachedBackend_Save_MetadataCacheClobberedDuringPersist_RestoresRealIDEntry(t *testing.T) {
	raw := newFakeRawBackend()
	raw.createdRealIDs = []string{"real-uuid-clobber"}
	raw.saveStarted = make(chan struct{})
	raw.saveRelease = make(chan struct{})

	backend := NewCachedBackend(raw, BackendOptions{
		CacheSecrets:  true,
		CacheMetadata: true,
		AsyncSave:     true,
	})

	item := &SecretItem{
		Label:      "test clobber item",
		Attributes: map[string]string{"service": "test:clobber"},
		Secret:     []byte("secret-clobber"),
	}

	if err := backend.Save(context.Background(), item); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	pendingID := item.ID
	<-raw.saveStarted

	// Simulate concurrent loadMetadataCache clobbering metaCache while raw.Save is in flight
	backend.metaMu.Lock()
	backend.metaCache = make(map[string]*SecretItem) // pendingID is now absent
	backend.metaLoaded = true
	backend.metaMu.Unlock()

	// Release raw.Save
	close(raw.saveRelease)

	// Wait for pending save to settle
	for i := 0; i < 50; i++ {
		backend.metaMu.Lock()
		pendingLen := len(backend.pendingSaves)
		backend.metaMu.Unlock()
		if pendingLen == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Verify that real-uuid-clobber was restored to metaCache and is visible via Search
	results, err := backend.Search(context.Background(), map[string]string{"service": "test:clobber"})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ID != "real-uuid-clobber" {
		t.Fatalf("expected item ID real-uuid-clobber, got %s", results[0].ID)
	}

	// Check metaCache directly
	backend.metaMu.Lock()
	cachedReal := backend.metaCache["real-uuid-clobber"]
	cachedPending := backend.metaCache[pendingID]
	backend.metaMu.Unlock()

	if cachedReal == nil {
		t.Fatalf("expected real-uuid-clobber in metaCache, got nil")
	}
	if cachedPending != nil {
		t.Fatalf("expected pendingID to be absent in metaCache, got %+v", cachedPending)
	}
}
