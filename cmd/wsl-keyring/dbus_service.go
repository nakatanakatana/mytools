package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

const (
	ServicePath    = "/org/freedesktop/secrets"
	CollectionPath = "/org/freedesktop/secrets/collection/default"
	SessionPath    = "/org/freedesktop/secrets/session/"
	ItemPathPrefix = "/org/freedesktop/secrets/collection/default/item/"

	ServiceInterface    = "org.freedesktop.Secret.Service"
	CollectionInterface = "org.freedesktop.Secret.Collection"
	ItemInterface       = "org.freedesktop.Secret.Item"
	SessionInterface    = "org.freedesktop.Secret.Session"
	PropertiesInterface = "org.freedesktop.DBus.Properties"

	AlgorithmPlain = "plain"
	AlgorithmDH    = "dh-ietf1024-sha256-aes128-cbc-pkcs7"

	ReplaceSearchTimeout = 250 * time.Millisecond
)

// DBusSecret represents the 'Secret' structure defined in Secret Service API spec.
// Signature: (oayays)
type DBusSecret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

// SessionState holds the state of an open session.
type SessionState struct {
	algorithm string
	aesKey    []byte // nil for plain sessions
}

// ServiceObject implements org.freedesktop.Secret.Service
type ServiceObject struct {
	conn     *dbus.Conn
	backend  StorageBackend
	mu       sync.Mutex
	sessions map[dbus.ObjectPath]*SessionState
	nextID   int
}

func NewServiceObject(conn *dbus.Conn, backend StorageBackend) *ServiceObject {
	return &ServiceObject{
		conn:     conn,
		backend:  backend,
		sessions: make(map[dbus.ObjectPath]*SessionState),
	}
}

func (s *ServiceObject) newSessionPath() dbus.ObjectPath {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	return dbus.ObjectPath(fmt.Sprintf("%s%d", SessionPath, id))
}

func (s *ServiceObject) getSession(path dbus.ObjectPath) (*SessionState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[path]
	return sess, ok
}

func (s *ServiceObject) OpenSession(algorithm string, input dbus.Variant) (dbus.Variant, dbus.ObjectPath, *dbus.Error) {
	switch algorithm {
	case AlgorithmPlain:
		path := s.newSessionPath()
		s.mu.Lock()
		s.sessions[path] = &SessionState{algorithm: AlgorithmPlain}
		s.mu.Unlock()

		sessionObj := &SessionObject{service: s, path: path}
		if err := s.conn.Export(sessionObj, path, SessionInterface); err != nil {
			s.mu.Lock()
			delete(s.sessions, path)
			s.mu.Unlock()
			return dbus.MakeVariant(""), dbus.ObjectPath("/"), dbus.NewError(
				"org.freedesktop.Secret.Error.Failed",
				[]any{fmt.Sprintf("failed to export session: %v", err)},
			)
		}

		return dbus.MakeVariant(""), path, nil

	case AlgorithmDH:
		// Client sends its DH public key as the input
		clientPublicBytes, ok := input.Value().([]byte)
		if !ok || len(clientPublicBytes) == 0 {
			return dbus.MakeVariant(""), dbus.ObjectPath("/"), dbus.NewError(
				"org.freedesktop.DBus.Error.InvalidArgs",
				[]any{"input must be client DH public key bytes"},
			)
		}

		// Generate server DH keypair
		serverKP, err := GenerateDHKeypair()
		if err != nil {
			return dbus.MakeVariant(""), dbus.ObjectPath("/"), dbus.NewError(
				"org.freedesktop.Secret.Error.Failed",
				[]any{fmt.Sprintf("failed to generate DH keypair: %v", err)},
			)
		}

		// Compute shared secret and derive AES key
		sharedSecret := ComputeSharedSecret(clientPublicBytes, serverKP.Private)
		aesKey, err := DeriveAESKey(sharedSecret)
		if err != nil {
			return dbus.MakeVariant(""), dbus.ObjectPath("/"), dbus.NewError(
				"org.freedesktop.Secret.Error.Failed",
				[]any{fmt.Sprintf("failed to derive AES key: %v", err)},
			)
		}

		path := s.newSessionPath()
		s.mu.Lock()
		s.sessions[path] = &SessionState{
			algorithm: AlgorithmDH,
			aesKey:    aesKey,
		}
		s.mu.Unlock()

		sessionObj := &SessionObject{service: s, path: path}
		if err := s.conn.Export(sessionObj, path, SessionInterface); err != nil {
			s.mu.Lock()
			delete(s.sessions, path)
			s.mu.Unlock()
			return dbus.MakeVariant(""), dbus.ObjectPath("/"), dbus.NewError(
				"org.freedesktop.Secret.Error.Failed",
				[]any{fmt.Sprintf("failed to export session: %v", err)},
			)
		}

		// Return server public key bytes as output
		serverPublicBytes := BigIntToBytes(serverKP.Public)
		return dbus.MakeVariant(serverPublicBytes), path, nil

	default:
		return dbus.MakeVariant(""), dbus.ObjectPath("/"), dbus.NewError(
			"org.freedesktop.Secret.Error.NotSupported",
			[]any{fmt.Sprintf("algorithm %q is not supported", algorithm)},
		)
	}
}

func (s *ServiceObject) CreateCollection(properties map[string]dbus.Variant, alias string) (dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
	// We only support the default collection
	return dbus.ObjectPath(CollectionPath), dbus.ObjectPath("/"), nil
}

func (s *ServiceObject) SearchItems(attributes map[string]string) ([]dbus.ObjectPath, []dbus.ObjectPath, *dbus.Error) {
	paths, dbusErr := s.searchUnlockedItems(attributes)
	if dbusErr != nil {
		return nil, nil, dbusErr
	}

	return paths, []dbus.ObjectPath{}, nil
}

func (s *ServiceObject) searchUnlockedItems(attributes map[string]string) ([]dbus.ObjectPath, *dbus.Error) {
	items, err := s.backend.Search(context.Background(), attributes)
	if err != nil {
		return nil, dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{err.Error()})
	}

	paths := make([]dbus.ObjectPath, len(items))
	for i, item := range items {
		paths[i] = dbus.ObjectPath(ItemPathPrefix + item.ID)
		if err := s.ExportItem(item); err != nil {
			return nil, dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{err.Error()})
		}
	}

	return paths, nil
}

func (s *ServiceObject) encryptSecret(sess *SessionState, plaintext []byte) (params []byte, value []byte, err error) {
	if sess.algorithm == AlgorithmDH && sess.aesKey != nil {
		iv, ciphertext, err := AESCBCEncrypt(sess.aesKey, plaintext)
		if err != nil {
			return nil, nil, err
		}
		return iv, ciphertext, nil
	}
	// plain: no encryption
	return []byte{}, plaintext, nil
}

func (s *ServiceObject) decryptSecret(sess *SessionState, params, value []byte) ([]byte, error) {
	if sess.algorithm == AlgorithmDH && sess.aesKey != nil {
		if len(params) != 16 {
			return nil, fmt.Errorf("expected 16-byte IV, got %d bytes", len(params))
		}
		return AESCBCDecrypt(sess.aesKey, params, value)
	}
	// plain: value is plaintext
	return value, nil
}

func (s *ServiceObject) GetSecrets(items []dbus.ObjectPath, session dbus.ObjectPath) (map[dbus.ObjectPath]DBusSecret, *dbus.Error) {
	sess, ok := s.getSession(session)
	if !ok {
		return nil, dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{"invalid session"})
	}

	secrets := make(map[dbus.ObjectPath]DBusSecret)
	for _, path := range items {
		id := strings.TrimPrefix(string(path), ItemPathPrefix)
		if id == string(path) {
			continue // Invalid path
		}

		item, err := s.backend.Get(context.Background(), id)
		if err != nil {
			continue // Skip if not found
		}

		params, value, err := s.encryptSecret(sess, item.Secret)
		if err != nil {
			continue
		}

		secrets[path] = DBusSecret{
			Session:     session,
			Parameters:  params,
			Value:       value,
			ContentType: "text/plain",
		}
	}
	return secrets, nil
}

func (s *ServiceObject) Lock(objects []dbus.ObjectPath) ([]dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
	return []dbus.ObjectPath{}, dbus.ObjectPath("/"), nil
}

func (s *ServiceObject) Unlock(objects []dbus.ObjectPath) ([]dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
	return objects, dbus.ObjectPath("/"), nil
}

func (s *ServiceObject) SetAlias(name string, collection dbus.ObjectPath) *dbus.Error {
	return nil
}

func (s *ServiceObject) ReadAlias(name string) (dbus.ObjectPath, *dbus.Error) {
	if name == "default" {
		return dbus.ObjectPath(CollectionPath), nil
	}
	return dbus.ObjectPath("/"), nil
}

// Properties implementation
func (s *ServiceObject) Get(iface, property string) (dbus.Variant, *dbus.Error) {
	if iface == ServiceInterface && property == "Collections" {
		return dbus.MakeVariant([]dbus.ObjectPath{dbus.ObjectPath(CollectionPath)}), nil
	}
	return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"No such property"})
}

func (s *ServiceObject) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface == ServiceInterface {
		return map[string]dbus.Variant{
			"Collections": dbus.MakeVariant([]dbus.ObjectPath{dbus.ObjectPath(CollectionPath)}),
		}, nil
	}
	return map[string]dbus.Variant{}, nil
}

func (s *ServiceObject) Set(iface, property string, value dbus.Variant) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.NotSupported", []any{"Property is read-only"})
}

func (s *ServiceObject) ExportItem(item *SecretItem) error {
	if s.conn == nil {
		return nil
	}
	path := dbus.ObjectPath(ItemPathPrefix + item.ID)
	itemObj := &ItemObject{
		backend: s.backend,
		service: s,
		id:      item.ID,
	}
	if err := s.conn.Export(itemObj, path, ItemInterface); err != nil {
		return fmt.Errorf("failed to export item interface for %s: %w", path, err)
	}
	if err := s.conn.Export(itemObj, path, PropertiesInterface); err != nil {
		return fmt.Errorf("failed to export item properties for %s: %w", path, err)
	}
	if err := s.conn.Export(introspect.Introspectable(itemIntrospectXML), path, "org.freedesktop.DBus.Introspectable"); err != nil {
		return fmt.Errorf("failed to export item introspection for %s: %w", path, err)
	}
	return nil
}

// CollectionObject implements org.freedesktop.Secret.Collection
type CollectionObject struct {
	conn    *dbus.Conn
	backend StorageBackend
	service *ServiceObject
}

func NewCollectionObject(conn *dbus.Conn, backend StorageBackend, service *ServiceObject) *CollectionObject {
	return &CollectionObject{
		conn:    conn,
		backend: backend,
		service: service,
	}
}

func (c *CollectionObject) CreateItem(properties map[string]dbus.Variant, secret DBusSecret, replace bool) (dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
	label := ""
	if variant, ok := properties[ItemInterface+".Label"]; ok {
		label = variant.Value().(string)
	}

	attributes := make(map[string]string)
	if variant, ok := properties[ItemInterface+".Attributes"]; ok {
		if attrs, ok := variant.Value().(map[string]string); ok {
			attributes = attrs
		}
	}
	// Decrypt the secret value if session is encrypted
	sess, ok := c.service.getSession(secret.Session)
	if !ok {
		return dbus.ObjectPath("/"), dbus.ObjectPath("/"), dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{"invalid session"})
	}
	secretValue, err := c.service.decryptSecret(sess, secret.Parameters, secret.Value)
	if err != nil {
		return dbus.ObjectPath("/"), dbus.ObjectPath("/"), dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{err.Error()})
	}

	// Try to find if we need to replace or update
	var id string
	if replace {
		searchCtx, cancel := context.WithTimeout(context.Background(), ReplaceSearchTimeout)
		defer cancel()
		items, err := c.backend.Search(searchCtx, attributes)
		if err == nil && len(items) > 0 {
			id = items[0].ID
		}
	}

	item := &SecretItem{
		ID:         id,
		Label:      label,
		Attributes: attributes,
		Secret:     secretValue,
	}

	if err := c.backend.Save(context.Background(), item); err != nil {
		return dbus.ObjectPath("/"), dbus.ObjectPath("/"), dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{err.Error()})
	}

	if err := c.service.ExportItem(item); err != nil {
		return dbus.ObjectPath("/"), dbus.ObjectPath("/"), dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{err.Error()})
	}

	return dbus.ObjectPath(ItemPathPrefix + item.ID), dbus.ObjectPath("/"), nil
}

func (c *CollectionObject) SearchItems(attributes map[string]string) ([]dbus.ObjectPath, *dbus.Error) {
	paths, dbusErr := c.service.searchUnlockedItems(attributes)
	if dbusErr != nil {
		return nil, dbusErr
	}

	return paths, nil
}

func (c *CollectionObject) Delete() (dbus.ObjectPath, *dbus.Error) {
	return dbus.ObjectPath("/"), dbus.NewError("org.freedesktop.DBus.Error.NotSupported", []any{"Default collection cannot be deleted"})
}

// Properties implementation
func (c *CollectionObject) Get(iface, property string) (dbus.Variant, *dbus.Error) {
	if iface != CollectionInterface {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"No such interface"})
	}

	switch property {
	case "Items":
		list, err := c.backend.List(context.Background())
		if err != nil {
			return dbus.Variant{}, dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{err.Error()})
		}
		paths := make([]dbus.ObjectPath, len(list))
		for i, item := range list {
			paths[i] = dbus.ObjectPath(ItemPathPrefix + item.ID)
		}
		return dbus.MakeVariant(paths), nil
	case "Label":
		return dbus.MakeVariant("Default"), nil
	case "Locked":
		return dbus.MakeVariant(false), nil
	case "Created":
		return dbus.MakeVariant(uint64(0)), nil
	case "Modified":
		return dbus.MakeVariant(uint64(0)), nil
	}
	return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"No such property"})
}

func (c *CollectionObject) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface != CollectionInterface {
		return map[string]dbus.Variant{}, nil
	}

	props := make(map[string]dbus.Variant)
	for _, prop := range []string{"Items", "Label", "Locked", "Created", "Modified"} {
		if val, err := c.Get(iface, prop); err == nil {
			props[prop] = val
		}
	}
	return props, nil
}

func (c *CollectionObject) Set(iface, property string, value dbus.Variant) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.NotSupported", []any{"Properties are read-only"})
}

// ItemObject implements org.freedesktop.Secret.Item
type ItemObject struct {
	backend StorageBackend
	service *ServiceObject
	id      string
}

func (item *ItemObject) GetSecret(session dbus.ObjectPath) (DBusSecret, *dbus.Error) {
	it, err := item.backend.Get(context.Background(), item.id)
	if err != nil {
		return DBusSecret{}, dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{err.Error()})
	}

	sess, ok := item.service.getSession(session)
	if !ok {
		return DBusSecret{}, dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{"invalid session"})
	}

	params, value, err := item.service.encryptSecret(sess, it.Secret)
	if err != nil {
		return DBusSecret{}, dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{err.Error()})
	}

	return DBusSecret{
		Session:     session,
		Parameters:  params,
		Value:       value,
		ContentType: "text/plain",
	}, nil
}

func (item *ItemObject) SetSecret(secret DBusSecret) *dbus.Error {
	it, err := item.backend.Get(context.Background(), item.id)
	if err != nil {
		return dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{err.Error()})
	}

	sess, ok := item.service.getSession(secret.Session)
	if !ok {
		return dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{"invalid session"})
	}
	secretValue, err := item.service.decryptSecret(sess, secret.Parameters, secret.Value)
	if err != nil {
		return dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{err.Error()})
	}

	it.Secret = secretValue
	if err := item.backend.Save(context.Background(), it); err != nil {
		return dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{err.Error()})
	}
	return nil
}

func (item *ItemObject) Delete() (dbus.ObjectPath, *dbus.Error) {
	if err := item.backend.Delete(context.Background(), item.id); err != nil {
		return dbus.ObjectPath("/"), dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{err.Error()})
	}
	return dbus.ObjectPath("/"), nil
}

// Properties implementation
func (item *ItemObject) Get(iface, property string) (dbus.Variant, *dbus.Error) {
	if iface != ItemInterface {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"No such interface"})
	}

	it, err := item.backend.Get(context.Background(), item.id)
	if err != nil {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.Secret.Error.Failed", []any{err.Error()})
	}

	switch property {
	case "Locked":
		return dbus.MakeVariant(false), nil
	case "Attributes":
		return dbus.MakeVariant(it.Attributes), nil
	case "Label":
		return dbus.MakeVariant(it.Label), nil
	case "Created":
		return dbus.MakeVariant(uint64(0)), nil
	case "Modified":
		return dbus.MakeVariant(uint64(0)), nil
	}
	return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"No such property"})
}

func (item *ItemObject) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface != ItemInterface {
		return map[string]dbus.Variant{}, nil
	}

	props := make(map[string]dbus.Variant)
	for _, prop := range []string{"Locked", "Attributes", "Label", "Created", "Modified"} {
		if val, err := item.Get(iface, prop); err == nil {
			props[prop] = val
		}
	}
	return props, nil
}

func (item *ItemObject) Set(iface, property string, value dbus.Variant) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.NotSupported", []any{"Properties are read-only"})
}

// SessionObject implements org.freedesktop.Secret.Session
type SessionObject struct {
	service *ServiceObject
	path    dbus.ObjectPath
}

func (s *SessionObject) Close() *dbus.Error {
	s.service.mu.Lock()
	delete(s.service.sessions, s.path)
	s.service.mu.Unlock()
	return nil
}
