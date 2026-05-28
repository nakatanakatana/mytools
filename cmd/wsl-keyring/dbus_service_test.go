package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/godbus/dbus/v5"
)

type recordingBackend struct {
	item      *SecretItem
	saveCalls int
}

func (b *recordingBackend) Search(ctx context.Context, attributes map[string]string) ([]*SecretItem, error) {
	return nil, nil
}

func (b *recordingBackend) Get(ctx context.Context, id string) (*SecretItem, error) {
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
	return nil
}

func (b *recordingBackend) List(ctx context.Context) ([]*SecretItem, error) {
	return nil, nil
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
		backend: backend,
		service: service,
		id:      "item1",
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
		backend: backend,
		service: service,
		id:      "item1",
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
