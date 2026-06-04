package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type recordingBackend struct {
	item       *SecretItem
	items      []*SecretItem
	getCalls   int
	saveCalls  int
	deleteID   string
	searchFunc func(ctx context.Context, attributes map[string]string) ([]*SecretItem, error)
}

func (b *recordingBackend) Search(ctx context.Context, attributes map[string]string) ([]*SecretItem, error) {
	if b.searchFunc != nil {
		return b.searchFunc(ctx, attributes)
	}
	return b.items, nil
}

func (b *recordingBackend) Get(ctx context.Context, id string) (*SecretItem, error) {
	b.getCalls++
	if b.item == nil || b.item.ID != id {
		return nil, ErrNotFound
	}
	return b.item, nil
}

func (b *recordingBackend) Save(ctx context.Context, item *SecretItem) error {
	b.saveCalls++
	b.item = item
	if b.item.ID == "" {
		b.item.ID = fmt.Sprintf("saved-%d", b.saveCalls)
	}
	return nil
}

func (b *recordingBackend) Delete(ctx context.Context, id string) error {
	b.deleteID = id
	return nil
}

func (b *recordingBackend) List(ctx context.Context) ([]*SecretItem, error) {
	return b.items, nil
}

func TestCollectionCreateItem_InvalidSessionDoesNotSave(t *testing.T) {
	backend := &recordingBackend{}
	service := NewServiceObject(nil, backend)
	collection := NewCollectionObject(nil, backend, service)

	itemPath, prompt, dbusErr := collection.CreateItem(
		map[string]dbus.Variant{
			ItemInterface + ".Label":      dbus.MakeVariant("label"),
			ItemInterface + ".Attributes": dbus.MakeVariant(map[string]string{"service": "test"}),
		},
		DBusSecret{
			Session: dbus.ObjectPath("/invalid/session"),
			Value:   []byte("secret"),
		},
		false,
	)

	if dbusErr == nil {
		t.Fatal("expected invalid session error")
	}
	if itemPath != dbus.ObjectPath("/") || prompt != dbus.ObjectPath("/") {
		t.Fatalf("unexpected paths: item=%s prompt=%s", itemPath, prompt)
	}
	if backend.saveCalls != 0 {
		t.Fatalf("Save was called %d times", backend.saveCalls)
	}
}

func TestServiceReadAliasReturnsDefaultCollection(t *testing.T) {
	service := NewServiceObject(nil, &recordingBackend{})

	path, dbusErr := service.ReadAlias("default")

	if dbusErr != nil {
		t.Fatalf("ReadAlias failed: %v", dbusErr)
	}
	if path != dbus.ObjectPath(CollectionPath) {
		t.Fatalf("ReadAlias(default) = %s, want %s", path, CollectionPath)
	}
}

func TestServiceReadAliasReturnsRootForUnknownAlias(t *testing.T) {
	service := NewServiceObject(nil, &recordingBackend{})

	path, dbusErr := service.ReadAlias("missing")

	if dbusErr != nil {
		t.Fatalf("ReadAlias failed: %v", dbusErr)
	}
	if path != dbus.ObjectPath("/") {
		t.Fatalf("ReadAlias(missing) = %s, want /", path)
	}
}

func TestServiceNewSessionPathIncrements(t *testing.T) {
	service := NewServiceObject(nil, &recordingBackend{})

	first := service.newSessionPath()
	second := service.newSessionPath()

	if first != dbus.ObjectPath(SessionPath+"0") {
		t.Fatalf("first session path = %s", first)
	}
	if second != dbus.ObjectPath(SessionPath+"1") {
		t.Fatalf("second session path = %s", second)
	}
}

func TestServiceOpenSessionUnsupportedAlgorithm(t *testing.T) {
	service := NewServiceObject(nil, &recordingBackend{})

	_, path, dbusErr := service.OpenSession("unsupported", dbus.MakeVariant(""))

	if dbusErr == nil {
		t.Fatal("expected unsupported algorithm error")
	}
	if path != dbus.ObjectPath("/") {
		t.Fatalf("session path = %s, want /", path)
	}
}

func TestServiceCreateCollectionReturnsDefaultCollection(t *testing.T) {
	service := NewServiceObject(nil, &recordingBackend{})

	collectionPath, prompt, dbusErr := service.CreateCollection(nil, "default")

	if dbusErr != nil {
		t.Fatalf("CreateCollection failed: %v", dbusErr)
	}
	if collectionPath != dbus.ObjectPath(CollectionPath) || prompt != dbus.ObjectPath("/") {
		t.Fatalf("CreateCollection = (%s, %s), want (%s, /)", collectionPath, prompt, CollectionPath)
	}
}

func TestServiceSearchItemsReturnsUnlockedPaths(t *testing.T) {
	backend := &recordingBackend{
		items: []*SecretItem{
			{ID: "item1", Label: "label", Attributes: map[string]string{"service": "github"}},
		},
	}
	service := NewServiceObject(nil, backend)

	unlocked, locked, dbusErr := service.SearchItems(map[string]string{"service": "github"})

	if dbusErr != nil {
		t.Fatalf("SearchItems failed: %v", dbusErr)
	}
	if len(locked) != 0 {
		t.Fatalf("locked paths = %v, want empty", locked)
	}
	want := dbus.ObjectPath(ItemPathPrefix + "item1")
	if len(unlocked) != 1 || unlocked[0] != want {
		t.Fatalf("unlocked paths = %v, want [%s]", unlocked, want)
	}
}

func TestServiceGetSecretsPlainSessionReturnsSecret(t *testing.T) {
	backend := &recordingBackend{
		item: &SecretItem{
			ID:     "item1",
			Secret: []byte("plain-secret"),
		},
	}
	service := NewServiceObject(nil, backend)
	sessionPath := dbus.ObjectPath(SessionPath + "plain")
	service.sessions[sessionPath] = &SessionState{algorithm: AlgorithmPlain}

	secrets, dbusErr := service.GetSecrets([]dbus.ObjectPath{
		dbus.ObjectPath(ItemPathPrefix + "item1"),
		dbus.ObjectPath("/invalid/path"),
	}, sessionPath)

	if dbusErr != nil {
		t.Fatalf("GetSecrets failed: %v", dbusErr)
	}
	path := dbus.ObjectPath(ItemPathPrefix + "item1")
	got, ok := secrets[path]
	if !ok {
		t.Fatalf("secret for %s was not returned: %+v", path, secrets)
	}
	if string(got.Value) != "plain-secret" || len(got.Parameters) != 0 || got.ContentType != "text/plain" {
		t.Fatalf("unexpected secret: %+v", got)
	}
	if got.Session != sessionPath {
		t.Fatalf("secret session = %s, want %s", got.Session, sessionPath)
	}
}

func TestServiceLockUnlockSetAliasAndProperties(t *testing.T) {
	service := NewServiceObject(nil, &recordingBackend{})
	objects := []dbus.ObjectPath{dbus.ObjectPath(CollectionPath)}

	locked, prompt, dbusErr := service.Lock(objects)
	if dbusErr != nil {
		t.Fatalf("Lock failed: %v", dbusErr)
	}
	if len(locked) != 0 || prompt != dbus.ObjectPath("/") {
		t.Fatalf("Lock = (%v, %s), want empty and /", locked, prompt)
	}

	unlocked, prompt, dbusErr := service.Unlock(objects)
	if dbusErr != nil {
		t.Fatalf("Unlock failed: %v", dbusErr)
	}
	if len(unlocked) != 1 || unlocked[0] != objects[0] || prompt != dbus.ObjectPath("/") {
		t.Fatalf("Unlock = (%v, %s), want original objects and /", unlocked, prompt)
	}
	if dbusErr := service.SetAlias("default", dbus.ObjectPath(CollectionPath)); dbusErr != nil {
		t.Fatalf("SetAlias failed: %v", dbusErr)
	}

	collections, dbusErr := service.Get(ServiceInterface, "Collections")
	if dbusErr != nil {
		t.Fatalf("Get Collections failed: %v", dbusErr)
	}
	paths := collections.Value().([]dbus.ObjectPath)
	if len(paths) != 1 || paths[0] != dbus.ObjectPath(CollectionPath) {
		t.Fatalf("Collections = %v", paths)
	}
	all, dbusErr := service.GetAll(ServiceInterface)
	if dbusErr != nil {
		t.Fatalf("GetAll failed: %v", dbusErr)
	}
	if _, ok := all["Collections"]; !ok {
		t.Fatalf("GetAll missing Collections: %+v", all)
	}
	if dbusErr := service.Set(ServiceInterface, "Collections", dbus.MakeVariant([]dbus.ObjectPath{})); dbusErr == nil {
		t.Fatal("expected service property Set to fail")
	}
}

func TestCollectionCreateItem_DecryptFailureDoesNotSave(t *testing.T) {
	backend := &recordingBackend{}
	service := NewServiceObject(nil, backend)
	sessionPath := dbus.ObjectPath(SessionPath + "test")
	service.sessions[sessionPath] = &SessionState{
		algorithm: AlgorithmDH,
		aesKey:    []byte("0123456789abcdef"),
	}
	collection := NewCollectionObject(nil, backend, service)

	_, _, dbusErr := collection.CreateItem(
		map[string]dbus.Variant{
			ItemInterface + ".Label": dbus.MakeVariant("label"),
		},
		DBusSecret{
			Session:    sessionPath,
			Parameters: []byte("short"),
			Value:      []byte("secret"),
		},
		false,
	)

	if dbusErr == nil {
		t.Fatal("expected decrypt failure")
	}
	if backend.saveCalls != 0 {
		t.Fatalf("Save was called %d times", backend.saveCalls)
	}
}

func TestCollectionCreateItem_ReplaceSearchTimeoutStillSaves(t *testing.T) {
	backend := &recordingBackend{}
	searchStarted := make(chan struct{})
	backend.searchFunc = func(ctx context.Context, attributes map[string]string) ([]*SecretItem, error) {
		close(searchStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	service := NewServiceObject(nil, backend)
	sessionPath := dbus.ObjectPath(SessionPath + "test")
	service.sessions[sessionPath] = &SessionState{algorithm: AlgorithmPlain}
	collection := NewCollectionObject(nil, backend, service)

	start := time.Now()
	itemPath, prompt, dbusErr := collection.CreateItem(
		map[string]dbus.Variant{
			ItemInterface + ".Label":      dbus.MakeVariant("label"),
			ItemInterface + ".Attributes": dbus.MakeVariant(map[string]string{"service": "test"}),
		},
		DBusSecret{
			Session: sessionPath,
			Value:   []byte("secret"),
		},
		true,
	)

	if dbusErr != nil {
		t.Fatalf("CreateItem failed: %v", dbusErr)
	}
	if itemPath == dbus.ObjectPath("/") || prompt != dbus.ObjectPath("/") {
		t.Fatalf("unexpected paths: item=%s prompt=%s", itemPath, prompt)
	}
	if backend.saveCalls != 1 {
		t.Fatalf("Save was called %d times", backend.saveCalls)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("CreateItem took %s, expected bounded replace search", elapsed)
	}
	select {
	case <-searchStarted:
	default:
		t.Fatal("replace search was not attempted")
	}
}

func TestCollectionPropertiesSearchAndDelete(t *testing.T) {
	backend := &recordingBackend{
		items: []*SecretItem{
			{ID: "item1", Label: "label", Attributes: map[string]string{"service": "github"}},
		},
	}
	service := NewServiceObject(nil, backend)
	collection := NewCollectionObject(nil, backend, service)

	paths, dbusErr := collection.SearchItems(map[string]string{"service": "github"})
	if dbusErr != nil {
		t.Fatalf("Collection SearchItems failed: %v", dbusErr)
	}
	if len(paths) != 1 || paths[0] != dbus.ObjectPath(ItemPathPrefix+"item1") {
		t.Fatalf("Collection SearchItems = %v", paths)
	}

	items, dbusErr := collection.Get(CollectionInterface, "Items")
	if dbusErr != nil {
		t.Fatalf("Collection Get Items failed: %v", dbusErr)
	}
	itemPaths := items.Value().([]dbus.ObjectPath)
	if len(itemPaths) != 1 || itemPaths[0] != dbus.ObjectPath(ItemPathPrefix+"item1") {
		t.Fatalf("Collection Items = %v", itemPaths)
	}

	label, dbusErr := collection.Get(CollectionInterface, "Label")
	if dbusErr != nil || label.Value().(string) != "Default" {
		t.Fatalf("Collection Label = %v, err=%v", label.Value(), dbusErr)
	}
	locked, dbusErr := collection.Get(CollectionInterface, "Locked")
	if dbusErr != nil || locked.Value().(bool) {
		t.Fatalf("Collection Locked = %v, err=%v", locked.Value(), dbusErr)
	}
	all, dbusErr := collection.GetAll(CollectionInterface)
	if dbusErr != nil {
		t.Fatalf("Collection GetAll failed: %v", dbusErr)
	}
	for _, prop := range []string{"Items", "Label", "Locked", "Created", "Modified"} {
		if _, ok := all[prop]; !ok {
			t.Fatalf("Collection GetAll missing %s: %+v", prop, all)
		}
	}
	if _, dbusErr := collection.Delete(); dbusErr == nil {
		t.Fatal("expected collection Delete to fail")
	}
	if dbusErr := collection.Set(CollectionInterface, "Label", dbus.MakeVariant("x")); dbusErr == nil {
		t.Fatal("expected collection Set to fail")
	}
}

func TestItemSetSecret_InvalidSessionDoesNotSave(t *testing.T) {
	backend := &recordingBackend{
		item: &SecretItem{
			ID:         "item1",
			Label:      "label",
			Attributes: map[string]string{"service": "test"},
			Secret:     []byte("old-secret"),
		},
	}
	service := NewServiceObject(nil, backend)
	item := &ItemObject{
		backend:    backend,
		service:    service,
		id:         "item1",
		label:      "label",
		attributes: map[string]string{"service": "test"},
	}

	dbusErr := item.SetSecret(DBusSecret{
		Session: dbus.ObjectPath("/invalid/session"),
		Value:   []byte("new-secret"),
	})

	if dbusErr == nil {
		t.Fatal("expected invalid session error")
	}
	if backend.saveCalls != 0 {
		t.Fatalf("Save was called %d times", backend.saveCalls)
	}
	if string(backend.item.Secret) != "old-secret" {
		t.Fatalf("secret changed to %q", string(backend.item.Secret))
	}
}

func TestItemSetSecret_DecryptFailureDoesNotSave(t *testing.T) {
	backend := &recordingBackend{
		item: &SecretItem{
			ID:         "item1",
			Label:      "label",
			Attributes: map[string]string{"service": "test"},
			Secret:     []byte("old-secret"),
		},
	}
	service := NewServiceObject(nil, backend)
	sessionPath := dbus.ObjectPath(SessionPath + "test")
	service.sessions[sessionPath] = &SessionState{
		algorithm: AlgorithmDH,
		aesKey:    []byte("0123456789abcdef"),
	}
	item := &ItemObject{
		backend:    backend,
		service:    service,
		id:         "item1",
		label:      "label",
		attributes: map[string]string{"service": "github"},
	}

	dbusErr := item.SetSecret(DBusSecret{
		Session:    sessionPath,
		Parameters: []byte("short"),
		Value:      []byte("new-secret"),
	})

	if dbusErr == nil {
		t.Fatal("expected decrypt failure")
	}
	if backend.saveCalls != 0 {
		t.Fatalf("Save was called %d times", backend.saveCalls)
	}
	if string(backend.item.Secret) != "old-secret" {
		t.Fatalf("secret changed to %q", string(backend.item.Secret))
	}
}

func TestItemSetSecret_UsesExportedMetadataWithoutReadingSecret(t *testing.T) {
	backend := &recordingBackend{}
	service := NewServiceObject(nil, backend)
	sessionPath := dbus.ObjectPath(SessionPath + "plain")
	service.sessions[sessionPath] = &SessionState{algorithm: AlgorithmPlain}
	item := &ItemObject{
		backend:    backend,
		service:    service,
		id:         "item1",
		label:      "label",
		attributes: map[string]string{"service": "github", "username": "alice"},
	}

	dbusErr := item.SetSecret(DBusSecret{
		Session: sessionPath,
		Value:   []byte("new-secret"),
	})

	if dbusErr != nil {
		t.Fatalf("SetSecret failed: %v", dbusErr)
	}
	if backend.getCalls != 0 {
		t.Fatalf("Get was called %d times, want 0", backend.getCalls)
	}
	if backend.saveCalls != 1 {
		t.Fatalf("Save was called %d times, want 1", backend.saveCalls)
	}
	if backend.item.ID != "item1" || backend.item.Label != "label" {
		t.Fatalf("saved item metadata = %+v", backend.item)
	}
	if backend.item.Attributes["service"] != "github" || backend.item.Attributes["username"] != "alice" {
		t.Fatalf("saved attributes = %+v", backend.item.Attributes)
	}
	if string(backend.item.Secret) != "new-secret" {
		t.Fatalf("saved secret = %q", backend.item.Secret)
	}
}

func TestItemGetSecretDeleteAndProperties(t *testing.T) {
	backend := &recordingBackend{
		item: &SecretItem{
			ID:         "item1",
			Label:      "label",
			Attributes: map[string]string{"service": "github"},
			Secret:     []byte("secret"),
		},
	}
	service := NewServiceObject(nil, backend)
	sessionPath := dbus.ObjectPath(SessionPath + "plain")
	service.sessions[sessionPath] = &SessionState{algorithm: AlgorithmPlain}
	item := &ItemObject{
		backend:    backend,
		service:    service,
		id:         "item1",
		label:      "label",
		attributes: map[string]string{"service": "github"},
	}

	secret, dbusErr := item.GetSecret(sessionPath)
	if dbusErr != nil {
		t.Fatalf("GetSecret failed: %v", dbusErr)
	}
	if string(secret.Value) != "secret" || secret.ContentType != "text/plain" {
		t.Fatalf("unexpected secret: %+v", secret)
	}

	label, dbusErr := item.Get(ItemInterface, "Label")
	if dbusErr != nil || label.Value().(string) != "label" {
		t.Fatalf("Item Label = %v, err=%v", label.Value(), dbusErr)
	}
	attrs, dbusErr := item.Get(ItemInterface, "Attributes")
	if dbusErr != nil || attrs.Value().(map[string]string)["service"] != "github" {
		t.Fatalf("Item Attributes = %v, err=%v", attrs.Value(), dbusErr)
	}
	locked, dbusErr := item.Get(ItemInterface, "Locked")
	if dbusErr != nil || locked.Value().(bool) {
		t.Fatalf("Item Locked = %v, err=%v", locked.Value(), dbusErr)
	}
	all, dbusErr := item.GetAll(ItemInterface)
	if dbusErr != nil {
		t.Fatalf("Item GetAll failed: %v", dbusErr)
	}
	for _, prop := range []string{"Locked", "Attributes", "Label", "Created", "Modified"} {
		if _, ok := all[prop]; !ok {
			t.Fatalf("Item GetAll missing %s: %+v", prop, all)
		}
	}
	if _, dbusErr := item.Delete(); dbusErr != nil {
		t.Fatalf("Item Delete failed: %v", dbusErr)
	}
	if backend.deleteID != "item1" {
		t.Fatalf("Delete id = %q, want item1", backend.deleteID)
	}
	if dbusErr := item.Set(ItemInterface, "Label", dbus.MakeVariant("new")); dbusErr == nil {
		t.Fatal("expected item Set to fail")
	}
}

func TestSessionCloseRemovesSession(t *testing.T) {
	service := NewServiceObject(nil, &recordingBackend{})
	sessionPath := dbus.ObjectPath(SessionPath + "plain")
	service.sessions[sessionPath] = &SessionState{algorithm: AlgorithmPlain}
	session := &SessionObject{service: service, path: sessionPath}

	if dbusErr := session.Close(); dbusErr != nil {
		t.Fatalf("Close failed: %v", dbusErr)
	}
	if _, ok := service.sessions[sessionPath]; ok {
		t.Fatal("session was not removed")
	}
}
