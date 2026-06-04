package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestSecretServiceOPCommandSequence(t *testing.T) {
	t.Run("Service.SearchItems lists metadata", func(t *testing.T) {
		h := newDBusOPSequenceHarness(t)
		h.op.listItems = []opListItem{h.op.metadataListItem("id1", "label", map[string]string{"service": "github"})}

		unlocked, locked, dbusErr := h.service.SearchItems(map[string]string{"service": "github"})

		if dbusErr != nil {
			t.Fatalf("SearchItems failed: %v", dbusErr)
		}
		if len(unlocked) != 1 || len(locked) != 0 {
			t.Fatalf("SearchItems returned unlocked=%v locked=%v", unlocked, locked)
		}
		h.op.assertCalls(t, "item list")
	})

	t.Run("Service.GetSecrets gets each requested item", func(t *testing.T) {
		h := newDBusOPSequenceHarness(t)
		h.op.items["id1"] = h.op.secretItem("id1", "label", map[string]string{"service": "github"}, "secret")

		secrets, dbusErr := h.service.GetSecrets([]dbus.ObjectPath{dbus.ObjectPath(ItemPathPrefix + "id1")}, h.session)

		if dbusErr != nil {
			t.Fatalf("GetSecrets failed: %v", dbusErr)
		}
		if len(secrets) != 1 {
			t.Fatalf("GetSecrets returned %d secrets, want 1", len(secrets))
		}
		h.op.assertCalls(t, "item get id1")
	})

	t.Run("Collection.SearchItems lists metadata", func(t *testing.T) {
		h := newDBusOPSequenceHarness(t)
		h.op.listItems = []opListItem{h.op.metadataListItem("id1", "label", map[string]string{"service": "github"})}

		paths, dbusErr := h.collection.SearchItems(map[string]string{"service": "github"})

		if dbusErr != nil {
			t.Fatalf("Collection SearchItems failed: %v", dbusErr)
		}
		if len(paths) != 1 {
			t.Fatalf("Collection SearchItems returned %v", paths)
		}
		h.op.assertCalls(t, "item list")
	})

	t.Run("Collection.Items property lists metadata", func(t *testing.T) {
		h := newDBusOPSequenceHarness(t)
		h.op.listItems = []opListItem{h.op.metadataListItem("id1", "label", map[string]string{"service": "github"})}

		value, dbusErr := h.collection.Get(CollectionInterface, "Items")

		if dbusErr != nil {
			t.Fatalf("Collection Items failed: %v", dbusErr)
		}
		paths := value.Value().([]dbus.ObjectPath)
		if len(paths) != 1 || paths[0] != dbus.ObjectPath(ItemPathPrefix+"id1") {
			t.Fatalf("Collection Items returned %v", paths)
		}
		h.op.assertCalls(t, "item list")
	})

	t.Run("Collection.CreateItem without replace lists then creates", func(t *testing.T) {
		h := newDBusOPSequenceHarness(t)

		itemPath, prompt, dbusErr := h.collection.CreateItem(
			h.properties("label", map[string]string{"service": "github"}),
			DBusSecret{Session: h.session, Value: []byte("secret")},
			false,
		)

		if dbusErr != nil {
			t.Fatalf("CreateItem failed: %v", dbusErr)
		}
		if itemPath != dbus.ObjectPath(ItemPathPrefix+"created-id") || prompt != dbus.ObjectPath("/") {
			t.Fatalf("CreateItem returned item=%s prompt=%s", itemPath, prompt)
		}
		h.op.assertCalls(t, "item list", "item create")
	})

	t.Run("Collection.CreateItem with replace match lists then edits", func(t *testing.T) {
		h := newDBusOPSequenceHarness(t)
		h.op.listItems = []opListItem{h.op.metadataListItem("id1", "label", map[string]string{"service": "github"})}

		itemPath, prompt, dbusErr := h.collection.CreateItem(
			h.properties("label", map[string]string{"service": "github"}),
			DBusSecret{Session: h.session, Value: []byte("secret")},
			true,
		)

		if dbusErr != nil {
			t.Fatalf("CreateItem replace failed: %v", dbusErr)
		}
		if itemPath != dbus.ObjectPath(ItemPathPrefix+"id1") || prompt != dbus.ObjectPath("/") {
			t.Fatalf("CreateItem replace returned item=%s prompt=%s", itemPath, prompt)
		}
		h.op.assertCalls(t, "item list", "item edit id1")
	})

	t.Run("Item.GetSecret gets the item", func(t *testing.T) {
		h := newDBusOPSequenceHarness(t)
		h.op.items["id1"] = h.op.secretItem("id1", "label", map[string]string{"service": "github"}, "secret")
		item := h.itemObject("id1", "label", map[string]string{"service": "github"})

		secret, dbusErr := item.GetSecret(h.session)

		if dbusErr != nil {
			t.Fatalf("GetSecret failed: %v", dbusErr)
		}
		if string(secret.Value) != "secret" {
			t.Fatalf("GetSecret returned secret %q", secret.Value)
		}
		h.op.assertCalls(t, "item get id1")
	})

	t.Run("Item metadata properties do not call op", func(t *testing.T) {
		h := newDBusOPSequenceHarness(t)
		item := h.itemObject("id1", "label", map[string]string{"service": "github"})

		label, dbusErr := item.Get(ItemInterface, "Label")
		if dbusErr != nil {
			t.Fatalf("Item Label failed: %v", dbusErr)
		}
		attrs, dbusErr := item.Get(ItemInterface, "Attributes")
		if dbusErr != nil {
			t.Fatalf("Item Attributes failed: %v", dbusErr)
		}
		if label.Value().(string) != "label" || attrs.Value().(map[string]string)["service"] != "github" {
			t.Fatalf("unexpected item properties: label=%v attrs=%v", label.Value(), attrs.Value())
		}
		h.op.assertCalls(t)
	})

	t.Run("Item.SetSecret edits without reading", func(t *testing.T) {
		h := newDBusOPSequenceHarness(t)
		item := h.itemObject("id1", "label", map[string]string{"service": "github"})

		dbusErr := item.SetSecret(DBusSecret{Session: h.session, Value: []byte("new-secret")})

		if dbusErr != nil {
			t.Fatalf("SetSecret failed: %v", dbusErr)
		}
		h.op.assertCalls(t, "item edit id1")
	})

	t.Run("Item.Delete deletes the item", func(t *testing.T) {
		h := newDBusOPSequenceHarness(t)
		item := h.itemObject("id1", "label", map[string]string{"service": "github"})

		prompt, dbusErr := item.Delete()

		if dbusErr != nil {
			t.Fatalf("Delete failed: %v", dbusErr)
		}
		if prompt != dbus.ObjectPath("/") {
			t.Fatalf("Delete prompt = %s, want /", prompt)
		}
		h.op.assertCalls(t, "item delete id1")
	})
}

func TestSecretServiceOPCommandSequenceWithCache(t *testing.T) {
	t.Run("SearchItems uses metadata cache after first list", func(t *testing.T) {
		h := newCachedDBusOPSequenceHarness(t)
		h.op.listItems = []opListItem{h.op.metadataListItem("id1", "label", map[string]string{"service": "github"})}

		if _, _, dbusErr := h.service.SearchItems(map[string]string{"service": "github"}); dbusErr != nil {
			t.Fatalf("first SearchItems failed: %v", dbusErr)
		}
		if _, _, dbusErr := h.service.SearchItems(map[string]string{"service": "github"}); dbusErr != nil {
			t.Fatalf("second SearchItems failed: %v", dbusErr)
		}

		h.op.assertCalls(t, "item list")
	})

	t.Run("GetSecret uses secret cache after first get", func(t *testing.T) {
		h := newCachedDBusOPSequenceHarness(t)
		h.op.items["id1"] = h.op.secretItem("id1", "label", map[string]string{"service": "github"}, "secret")
		item := h.itemObject("id1", "label", map[string]string{"service": "github"})

		if _, dbusErr := item.GetSecret(h.session); dbusErr != nil {
			t.Fatalf("first GetSecret failed: %v", dbusErr)
		}
		if _, dbusErr := item.GetSecret(h.session); dbusErr != nil {
			t.Fatalf("second GetSecret failed: %v", dbusErr)
		}

		h.op.assertCalls(t, "item get id1")
	})

	t.Run("SetSecret updates secret cache so next GetSecret avoids get", func(t *testing.T) {
		h := newCachedDBusOPSequenceHarness(t)
		item := h.itemObject("id1", "label", map[string]string{"service": "github"})

		if dbusErr := item.SetSecret(DBusSecret{Session: h.session, Value: []byte("new-secret")}); dbusErr != nil {
			t.Fatalf("SetSecret failed: %v", dbusErr)
		}
		secret, dbusErr := item.GetSecret(h.session)
		if dbusErr != nil {
			t.Fatalf("GetSecret after SetSecret failed: %v", dbusErr)
		}
		if string(secret.Value) != "new-secret" {
			t.Fatalf("cached secret after SetSecret = %q", secret.Value)
		}

		h.op.assertCalls(t, "item edit id1")
	})

	t.Run("CreateItem updates metadata and secret cache", func(t *testing.T) {
		h := newCachedDBusOPSequenceHarness(t)

		itemPath, _, dbusErr := h.collection.CreateItem(
			h.properties("label", map[string]string{"service": "github"}),
			DBusSecret{Session: h.session, Value: []byte("new-secret")},
			false,
		)
		if dbusErr != nil {
			t.Fatalf("CreateItem failed: %v", dbusErr)
		}
		if _, _, dbusErr := h.service.SearchItems(map[string]string{"service": "github"}); dbusErr != nil {
			t.Fatalf("SearchItems after CreateItem failed: %v", dbusErr)
		}
		id := strings.TrimPrefix(string(itemPath), ItemPathPrefix)
		item := h.itemObject(id, "label", map[string]string{"service": "github"})
		secret, dbusErr := item.GetSecret(h.session)
		if dbusErr != nil {
			t.Fatalf("GetSecret after CreateItem failed: %v", dbusErr)
		}
		if string(secret.Value) != "new-secret" {
			t.Fatalf("cached secret after CreateItem = %q", secret.Value)
		}

		h.op.assertCalls(t, "item list", "item create")
	})
}

type dbusOPSequenceHarness struct {
	op         *opCommandRecorder
	backend    *OnePasswordBackend
	service    *ServiceObject
	collection *CollectionObject
	session    dbus.ObjectPath
}

func newDBusOPSequenceHarness(t *testing.T) *dbusOPSequenceHarness {
	t.Helper()

	op := &opCommandRecorder{
		items: make(map[string]opItem),
	}
	backend := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "wsl-keyring",
		runCmd: op.runCmd,
	}
	service := NewServiceObject(nil, backend)
	session := dbus.ObjectPath(SessionPath + "plain")
	service.sessions[session] = &SessionState{algorithm: AlgorithmPlain}

	return &dbusOPSequenceHarness{
		op:         op,
		backend:    backend,
		service:    service,
		collection: NewCollectionObject(nil, backend, service),
		session:    session,
	}
}

func newCachedDBusOPSequenceHarness(t *testing.T) *dbusOPSequenceHarness {
	t.Helper()

	h := newDBusOPSequenceHarness(t)
	cached := NewCachedBackend(h.backend, BackendOptions{
		CacheSecrets:        true,
		CacheMetadata:       true,
		SecretCacheTTL:      time.Hour,
		AuthCheckMinSpacing: time.Hour,
	})
	h.service = NewServiceObject(nil, cached)
	h.service.sessions[h.session] = &SessionState{algorithm: AlgorithmPlain}
	h.collection = NewCollectionObject(nil, cached, h.service)
	return h
}

func (h *dbusOPSequenceHarness) properties(label string, attrs map[string]string) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		ItemInterface + ".Label":      dbus.MakeVariant(label),
		ItemInterface + ".Attributes": dbus.MakeVariant(attrs),
	}
}

func (h *dbusOPSequenceHarness) itemObject(id, label string, attrs map[string]string) *ItemObject {
	return &ItemObject{
		backend:    h.service.backend,
		service:    h.service,
		id:         id,
		label:      label,
		attributes: copyAttributes(attrs),
	}
}

type opCommandRecorder struct {
	calls     []string
	listItems []opListItem
	items     map[string]opItem
}

func (r *opCommandRecorder) runCmd(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
	if name != "op.exe" {
		return nil, fmt.Errorf("unexpected binary %q", name)
	}
	if len(args) < 2 || args[0] != "item" {
		return nil, fmt.Errorf("unexpected command: %s", strings.Join(args, " "))
	}

	switch args[1] {
	case "list":
		r.calls = append(r.calls, "item list")
		return json.Marshal(r.listItems)
	case "get":
		if len(args) < 3 {
			return nil, fmt.Errorf("missing item id: %s", strings.Join(args, " "))
		}
		id := args[2]
		r.calls = append(r.calls, "item get "+id)
		item, ok := r.items[id]
		if !ok {
			return nil, fmt.Errorf("missing fake item %s", id)
		}
		return json.Marshal(item)
	case "create":
		r.calls = append(r.calls, "item create")
		return json.Marshal(opItem{ID: "created-id", Title: "created"})
	case "edit":
		if len(args) < 3 {
			return nil, fmt.Errorf("missing item id: %s", strings.Join(args, " "))
		}
		id := args[2]
		r.calls = append(r.calls, "item edit "+id)
		return json.Marshal(opItem{ID: id, Title: "edited"})
	case "delete":
		if len(args) < 3 {
			return nil, fmt.Errorf("missing item id: %s", strings.Join(args, " "))
		}
		id := args[2]
		r.calls = append(r.calls, "item delete "+id)
		return []byte(`{}`), nil
	default:
		return nil, fmt.Errorf("unexpected command: %s", strings.Join(args, " "))
	}
}

func (r *opCommandRecorder) metadataListItem(id, label string, attrs map[string]string) opListItem {
	return opListItem{
		ID:    id,
		Title: label,
		Tags:  buildOPMetadataTags(attrs),
	}
}

func (r *opCommandRecorder) secretItem(id, label string, attrs map[string]string, secret string) opItem {
	attrsJSON, _ := json.Marshal(attrs)
	return opItem{
		ID:    id,
		Title: label,
		Fields: []opItemField{
			{ID: "username", Label: "username", Type: "STRING", Value: attrs["username"]},
			{ID: "password", Label: "password", Type: "CONCEALED", Value: secret},
			{ID: "attributes", Label: "attributes", Type: "STRING", Value: string(attrsJSON)},
		},
	}
}

func (r *opCommandRecorder) assertCalls(t *testing.T, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("op calls = %v, want %v", r.calls, want)
	}
}
