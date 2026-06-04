package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func newAuthenticatedTestOnePasswordBackend() *OnePasswordBackend {
	b := &OnePasswordBackend{
		binary:       "op.exe",
		vault:        "test-vault",
		authCacheTTL: time.Hour,
	}
	b.markAuthSucceeded()
	return b
}

func TestInMemoryBackend(t *testing.T) {
	b := NewInMemoryBackend()
	ctx := context.Background()

	item := &SecretItem{
		ID:    "id1",
		Label: "label1",
		Attributes: map[string]string{
			"app":      "test",
			"username": "alice",
		},
		Secret: []byte("pass123"),
	}

	// Save
	if err := b.Save(ctx, item); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Get
	retrieved, err := b.Get(ctx, "id1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if retrieved.Label != "label1" || string(retrieved.Secret) != "pass123" {
		t.Errorf("Get returned unexpected item: %+v", retrieved)
	}

	// Search
	matched, err := b.Search(ctx, map[string]string{"app": "test"})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(matched) != 1 || matched[0].ID != "id1" {
		t.Errorf("Search returned unexpected results: %d items", len(matched))
	}

	// List
	list, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("List returned %d items, expected 1", len(list))
	}

	// Delete
	if err := b.Delete(ctx, "id1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Get after delete
	_, err = b.Get(ctx, "id1")
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestInMemoryBackend_LoadMetadataOmitsSecrets(t *testing.T) {
	b := NewInMemoryBackend()
	ctx := context.Background()
	if err := b.Save(ctx, &SecretItem{
		ID:         "id1",
		Label:      "label",
		Attributes: map[string]string{"service": "github"},
		Secret:     []byte("secret"),
	}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	items, err := b.LoadMetadata(ctx)
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("LoadMetadata returned %d items, want 1", len(items))
	}
	if items[0].ID != "id1" || items[0].Secret != nil {
		t.Fatalf("unexpected metadata item: %+v", items[0])
	}
}

func TestOnePasswordBackend_Save_CreatePersistsInBackground(t *testing.T) {
	b := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "test-vault",
	}

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		if name != "op.exe" {
			t.Errorf("expected op.exe, got %s", name)
		}

		// Check basic arguments
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "whoami") {
			return []byte(`{"user_uuid":"user"}`), nil
		}
		if strings.Contains(argsStr, "item list") {
			return []byte(`[]`), nil
		}
		close(started)

		if !strings.Contains(argsStr, "item create") {
			t.Errorf("expected item create, got: %s", argsStr)
		}
		if !strings.Contains(argsStr, "--vault test-vault") {
			t.Errorf("expected vault parameter, got: %s", argsStr)
		}
		if strings.Contains(argsStr, "username=bob") || strings.Contains(argsStr, "password=secret123") {
			t.Errorf("secret fields must be passed via stdin, got args: %s", argsStr)
		}
		if !strings.Contains(argsStr, "item create -") {
			t.Errorf("expected stdin template marker, got: %s", argsStr)
		}
		if !strings.Contains(argsStr, "--tags wsl-keyring,wsl-keyring-meta-v1") {
			t.Errorf("expected metadata tags, got: %s", argsStr)
		}
		if !strings.Contains(argsStr, "wsl-keyring-attr:YXBw=dnNjb2Rl") || !strings.Contains(argsStr, "wsl-keyring-attr:dXNlcm5hbWU=Ym9i") {
			t.Errorf("expected attribute tags, got: %s", argsStr)
		}
		if strings.Contains(argsStr, "--category") {
			t.Errorf("category must be provided only in stdin template, got args: %s", argsStr)
		}

		var template opItem
		if err := json.Unmarshal([]byte(stdin), &template); err != nil {
			t.Fatalf("failed to parse stdin template: %v", err)
		}
		if template.Title != "My Label" {
			t.Errorf("expected title in template, got %q", template.Title)
		}
		if template.Category != "LOGIN" {
			t.Errorf("expected LOGIN category, got %q", template.Category)
		}
		fields := map[string]string{}
		fieldMeta := map[string]opItemField{}
		for _, field := range template.Fields {
			fields[field.ID] = field.Value
			fieldMeta[field.ID] = field
		}
		if fields["username"] != "bob" || fields["password"] != "secret123" {
			t.Errorf("unexpected template fields: %+v", fields)
		}
		if fieldMeta["username"].Purpose != "USERNAME" || fieldMeta["username"].Type != "STRING" {
			t.Errorf("unexpected username field metadata: %+v", fieldMeta["username"])
		}
		if fieldMeta["password"].Purpose != "PASSWORD" || fieldMeta["password"].Type != "CONCEALED" {
			t.Errorf("unexpected password field metadata: %+v", fieldMeta["password"])
		}
		if !strings.Contains(fields["attributes"], "app=vscode") || !strings.Contains(fields["attributes"], "username=bob") {
			t.Errorf("unexpected attributes field: %q", fields["attributes"])
		}
		if !containsAll(template.Tags, []string{
			"wsl-keyring",
			"wsl-keyring-meta-v1",
			"wsl-keyring-attr:YXBw=dnNjb2Rl",
			"wsl-keyring-attr:dXNlcm5hbWU=Ym9i",
		}) {
			t.Errorf("unexpected template tags: %+v", template.Tags)
		}

		resp := opItem{
			ID:    "generated-uuid-5555",
			Title: "My Label",
		}
		<-release
		return json.Marshal(resp)
	}
	backend := NewCachedBackend(b, BackendOptions{
		CacheSecrets:        true,
		CacheMetadata:       true,
		AsyncSave:           true,
		AuthCheckMinSpacing: time.Hour,
	})

	item := &SecretItem{
		Label: "My Label",
		Attributes: map[string]string{
			"username": "bob",
			"app":      "vscode",
		},
		Secret: []byte("secret123"),
	}

	saveDone := make(chan error, 1)
	go func() {
		saveDone <- backend.Save(context.Background(), item)
	}()

	select {
	case err := <-saveDone:
		if err != nil {
			t.Fatalf("Save failed: %v", err)
		}
	case <-started:
		select {
		case err := <-saveDone:
			if err != nil {
				t.Fatalf("Save failed: %v", err)
			}
		case <-time.After(50 * time.Millisecond):
			close(release)
			t.Fatal("Save blocked waiting for op create")
		}
	case <-time.After(time.Second):
		t.Fatal("op create did not start")
	}
	if item.ID == "" {
		t.Fatal("expected Save to assign a pending ID")
	}

	got, err := backend.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("Get from cache after Save failed: %v", err)
	}
	if string(got.Secret) != "secret123" {
		t.Fatalf("cached secret = %q", string(got.Secret))
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background op create did not start")
	}
	close(release)
	go func() {
		for {
			matches, err := backend.Search(context.Background(), map[string]string{
				"username": "bob",
				"app":      "vscode",
			})
			if err == nil && len(matches) == 1 && matches[0].ID == "generated-uuid-5555" {
				close(done)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for background op create to reconcile item ID")
	}
}

func TestOnePasswordBackend_Save_CreateDoesNotRequireWhoamiPreflight(t *testing.T) {
	b := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "test-vault",
	}

	var commands []string
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		commands = append(commands, argsStr)
		if strings.Contains(argsStr, "whoami") {
			t.Fatalf("Save should let op item commands handle interactive authentication, got preflight: %s", argsStr)
		}
		switch {
		case strings.Contains(argsStr, "item list"):
			return []byte(`[]`), nil
		case strings.Contains(argsStr, "item create"):
			return []byte(`{"id":"generated-id","title":"My Label"}`), nil
		default:
			return nil, fmt.Errorf("unexpected command: %s", argsStr)
		}
	}

	item := &SecretItem{
		Label:      "My Label",
		Attributes: map[string]string{"app": "vscode", "username": "bob"},
		Secret:     []byte("secret123"),
	}
	if err := b.Save(context.Background(), item); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if item.ID != "generated-id" {
		t.Fatalf("item ID = %q, want generated-id", item.ID)
	}
	if len(commands) != 2 {
		t.Fatalf("commands = %+v, want item list and item create only", commands)
	}
}

func TestOnePasswordBackend_ReadsDoNotRequireWhoamiPreflight(t *testing.T) {
	b := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "test-vault",
	}

	var commands []string
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		commands = append(commands, argsStr)
		if strings.Contains(argsStr, "whoami") {
			t.Fatalf("reads should let op item commands handle interactive authentication, got preflight: %s", argsStr)
		}
		switch {
		case strings.Contains(argsStr, "item get"):
			return json.Marshal(opItem{
				ID:    args[2],
				Title: "Secret",
				Tags:  []string{"wsl-keyring"},
				Fields: []opItemField{
					{ID: "password", Type: "CONCEALED", Value: "secret"},
					{ID: "attributes", Type: "STRING", Value: "service=github"},
				},
			})
		case strings.Contains(argsStr, "item list"):
			return json.Marshal([]opListItem{{
				ID:    "id1",
				Title: "Secret",
				Tags:  []string{"wsl-keyring", "wsl-keyring-meta-v1", "wsl-keyring-attr:c2VydmljZQ=Z2l0aHVi"},
			}})
		default:
			return nil, fmt.Errorf("unexpected command: %s", argsStr)
		}
	}

	item, err := b.Get(context.Background(), "id1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if string(item.Secret) != "secret" {
		t.Fatalf("secret = %q, want secret", item.Secret)
	}

	items, err := b.LoadMetadata(context.Background())
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}
	if len(items) != 1 || items[0].ID != "id1" {
		t.Fatalf("LoadMetadata returned %+v, want id1", items)
	}
	if len(commands) != 2 {
		t.Fatalf("commands = %+v, want item get and item list only", commands)
	}
}

func containsAll(got []string, want []string) bool {
	values := make(map[string]bool, len(got))
	for _, value := range got {
		values[value] = true
	}
	for _, value := range want {
		if !values[value] {
			return false
		}
	}
	return true
}

func TestOnePasswordBackend_Get(t *testing.T) {
	b := newAuthenticatedTestOnePasswordBackend()

	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")

		if !strings.Contains(argsStr, "item get test-id") {
			return nil, fmt.Errorf("unexpected command: %s", argsStr)
		}

		attrsJSON := `{"app":"vscode","env":"dev"}`
		resp := opItem{
			ID:    "test-id",
			Title: "My Saved Secret",
			Fields: []opItemField{
				{ID: "username", Type: "STRING", Value: "alice"},
				{ID: "password", Type: "CONCEALED", Value: "supersecure"},
				{ID: "random-id-123", Label: "attributes", Type: "STRING", Value: attrsJSON},
			},
		}
		return json.Marshal(resp)
	}

	item, err := b.Get(context.Background(), "test-id")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if item.ID != "test-id" {
		t.Errorf("expected test-id, got %s", item.ID)
	}
	if item.Label != "My Saved Secret" {
		t.Errorf("expected My Saved Secret, got %s", item.Label)
	}
	if string(item.Secret) != "supersecure" {
		t.Errorf("expected supersecure, got %s", string(item.Secret))
	}
	if item.Attributes["username"] != "alice" || item.Attributes["app"] != "vscode" || item.Attributes["env"] != "dev" {
		t.Errorf("unexpected attributes: %+v", item.Attributes)
	}
}

func TestOnePasswordBackend_CheckAuth_CoalescesConcurrentChecks(t *testing.T) {
	b := &OnePasswordBackend{
		binary:       "op.exe",
		vault:        "test-vault",
		authCacheTTL: time.Minute,
	}

	whoamiStarted := make(chan struct{})
	releaseWhoami := make(chan struct{})
	var closeWhoamiStarted sync.Once

	var mu sync.Mutex
	whoamiCalls := 0

	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "whoami") {
			mu.Lock()
			whoamiCalls++
			mu.Unlock()
			closeWhoamiStarted.Do(func() { close(whoamiStarted) })
			<-releaseWhoami
			return []byte(`{"user_uuid":"user"}`), nil
		}

		return nil, fmt.Errorf("unexpected command: %s", argsStr)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := b.CheckAuth(context.Background()); err != nil {
				errs <- err
			}
		}()
	}

	close(start)
	select {
	case <-whoamiStarted:
	case <-time.After(time.Second):
		t.Fatal("op whoami did not start")
	}

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	gotWhoamiWhileBlocked := whoamiCalls
	mu.Unlock()
	if gotWhoamiWhileBlocked != 1 {
		t.Fatalf("whoami calls while auth blocked = %d, want 1", gotWhoamiWhileBlocked)
	}

	close(releaseWhoami)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if whoamiCalls != 1 {
		t.Fatalf("whoami calls = %d, want 1", whoamiCalls)
	}
}

func TestOnePasswordBackend_Get_CoalescesConcurrentIdenticalReadCommands(t *testing.T) {
	b := newAuthenticatedTestOnePasswordBackend()

	itemGetStarted := make(chan struct{})
	releaseItemGet := make(chan struct{})
	var closeItemGetStarted sync.Once

	var mu sync.Mutex
	itemGetCalls := 0

	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "whoami") {
			t.Fatalf("test backend should already be authenticated, got auth command: %s", argsStr)
		}
		if strings.Contains(argsStr, "item get same-id") {
			mu.Lock()
			itemGetCalls++
			mu.Unlock()
			closeItemGetStarted.Do(func() { close(itemGetStarted) })
			<-releaseItemGet
			return json.Marshal(opItem{
				ID:    "same-id",
				Title: "Secret",
				Fields: []opItemField{
					{ID: "password", Type: "CONCEALED", Value: "secret"},
				},
			})
		}
		return nil, fmt.Errorf("unexpected command: %s", argsStr)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			item, err := b.Get(context.Background(), "same-id")
			if err != nil {
				errs <- err
				return
			}
			if item.ID != "same-id" || string(item.Secret) != "secret" {
				errs <- fmt.Errorf("unexpected item: %+v", item)
			}
		}()
	}

	close(start)
	select {
	case <-itemGetStarted:
	case <-time.After(time.Second):
		t.Fatal("op item get did not start")
	}

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	gotCallsWhileBlocked := itemGetCalls
	mu.Unlock()
	if gotCallsWhileBlocked != 1 {
		t.Fatalf("item get calls while read blocked = %d, want 1", gotCallsWhileBlocked)
	}

	close(releaseItemGet)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if itemGetCalls != 1 {
		t.Fatalf("item get calls = %d, want 1", itemGetCalls)
	}
}

func TestOnePasswordBackend_Get_DoesNotCacheCompletedReadCommands(t *testing.T) {
	b := newAuthenticatedTestOnePasswordBackend()

	var mu sync.Mutex
	itemGetCalls := 0
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "whoami") {
			t.Fatalf("test backend should already be authenticated, got auth command: %s", argsStr)
		}
		if strings.Contains(argsStr, "item get same-id") {
			mu.Lock()
			itemGetCalls++
			mu.Unlock()
			return json.Marshal(opItem{
				ID:    "same-id",
				Title: "Secret",
				Fields: []opItemField{
					{ID: "password", Type: "CONCEALED", Value: "secret"},
				},
			})
		}
		return nil, fmt.Errorf("unexpected command: %s", argsStr)
	}

	if _, err := b.Get(context.Background(), "same-id"); err != nil {
		t.Fatalf("first Get failed: %v", err)
	}
	if _, err := b.Get(context.Background(), "same-id"); err != nil {
		t.Fatalf("second Get failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if itemGetCalls != 2 {
		t.Fatalf("item get calls = %d, want 2", itemGetCalls)
	}
}

func TestOnePasswordBackend_Save_DoesNotCoalesceWriteCommands(t *testing.T) {
	b := newAuthenticatedTestOnePasswordBackend()

	firstEditStarted := make(chan struct{})
	secondEditStarted := make(chan struct{})
	releaseEdits := make(chan struct{})

	var mu sync.Mutex
	editCalls := 0
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "whoami") {
			t.Fatalf("test backend should already be authenticated, got auth command: %s", argsStr)
		}
		if strings.Contains(argsStr, "item edit same-id") {
			mu.Lock()
			editCalls++
			call := editCalls
			mu.Unlock()
			if call == 1 {
				close(firstEditStarted)
			}
			if call == 2 {
				close(secondEditStarted)
			}
			<-releaseEdits
			return []byte(`{"id":"same-id","title":"Secret"}`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", argsStr)
	}

	item := &SecretItem{
		ID:         "same-id",
		Label:      "Secret",
		Attributes: map[string]string{"username": "alice"},
		Secret:     []byte("secret"),
	}

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- b.Save(context.Background(), copySecretItem(item))
		}()
	}

	select {
	case <-firstEditStarted:
	case <-time.After(time.Second):
		t.Fatal("first op item edit did not start")
	}
	select {
	case <-secondEditStarted:
	case <-time.After(time.Second):
		t.Fatal("second op item edit did not start; write command may have been coalesced")
	}

	close(releaseEdits)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if editCalls != 2 {
		t.Fatalf("item edit calls = %d, want 2", editCalls)
	}
}

func TestOnePasswordBackend_CheckAuth_DoesNotBlockGet(t *testing.T) {
	b := &OnePasswordBackend{
		binary:       "op.exe",
		vault:        "test-vault",
		authCacheTTL: time.Minute,
	}

	whoamiStarted := make(chan struct{})
	releaseWhoami := make(chan struct{})
	var closeWhoamiStarted sync.Once

	var mu sync.Mutex
	whoamiCalls := 0
	itemGetCalls := 0

	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "whoami") {
			mu.Lock()
			whoamiCalls++
			mu.Unlock()
			closeWhoamiStarted.Do(func() { close(whoamiStarted) })
			<-releaseWhoami
			return []byte(`{"user_uuid":"user"}`), nil
		}

		if strings.Contains(argsStr, "item get") {
			mu.Lock()
			itemGetCalls++
			mu.Unlock()
			return json.Marshal(opItem{
				ID:    args[2],
				Title: "Secret",
				Fields: []opItemField{
					{ID: "password", Type: "CONCEALED", Value: "secret"},
				},
			})
		}

		return nil, fmt.Errorf("unexpected command: %s", argsStr)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if err := b.CheckAuth(context.Background()); err != nil {
			errs <- err
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		item, err := b.Get(context.Background(), "id1")
		if err != nil {
			errs <- err
			return
		}
		if item.ID != "id1" || string(item.Secret) != "secret" {
			errs <- fmt.Errorf("unexpected item: %+v", item)
		}
	}()

	close(start)
	select {
	case <-whoamiStarted:
	case <-time.After(time.Second):
		t.Fatal("op whoami did not start")
	}

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	gotWhoamiWhileBlocked := whoamiCalls
	gotItemGetWhileBlocked := itemGetCalls
	mu.Unlock()
	if gotWhoamiWhileBlocked != 1 {
		t.Fatalf("whoami calls while auth blocked = %d, want 1", gotWhoamiWhileBlocked)
	}
	if gotItemGetWhileBlocked != 1 {
		t.Fatalf("item get calls while auth blocked = %d, want 1", gotItemGetWhileBlocked)
	}

	close(releaseWhoami)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if whoamiCalls != 1 {
		t.Fatalf("whoami calls = %d, want 1", whoamiCalls)
	}
	if itemGetCalls != 1 {
		t.Fatalf("item get calls = %d, want 1", itemGetCalls)
	}
}

func TestOnePasswordBackend_CheckAuth_ReusesRecentSuccess(t *testing.T) {
	b := &OnePasswordBackend{
		binary:       "op.exe",
		vault:        "test-vault",
		authCacheTTL: time.Minute,
	}

	var mu sync.Mutex
	whoamiCalls := 0
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "whoami") {
			mu.Lock()
			whoamiCalls++
			mu.Unlock()
			return []byte(`{"user_uuid":"user"}`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", argsStr)
	}

	if err := b.CheckAuth(context.Background()); err != nil {
		t.Fatalf("first CheckAuth failed: %v", err)
	}
	if err := b.CheckAuth(context.Background()); err != nil {
		t.Fatalf("second CheckAuth failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if whoamiCalls != 1 {
		t.Fatalf("whoami calls = %d, want 1", whoamiCalls)
	}
}

func TestOnePasswordBackend_CheckAuth_ZeroAuthCacheTTLDisablesSequentialReuse(t *testing.T) {
	b := &OnePasswordBackend{
		binary:       "op.exe",
		vault:        "test-vault",
		authCacheTTL: 0,
	}

	var mu sync.Mutex
	whoamiCalls := 0
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "whoami") {
			mu.Lock()
			whoamiCalls++
			mu.Unlock()
			return []byte(`{"user_uuid":"user"}`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", argsStr)
	}

	if err := b.CheckAuth(context.Background()); err != nil {
		t.Fatalf("first CheckAuth failed: %v", err)
	}
	if err := b.CheckAuth(context.Background()); err != nil {
		t.Fatalf("second CheckAuth failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if whoamiCalls != 2 {
		t.Fatalf("whoami calls = %d, want 2", whoamiCalls)
	}
}

func TestOnePasswordBackend_CheckAuth_ZeroAuthCacheTTLStillCoalescesConcurrentChecks(t *testing.T) {
	b := &OnePasswordBackend{
		binary:       "op.exe",
		vault:        "test-vault",
		authCacheTTL: 0,
	}

	whoamiStarted := make(chan struct{})
	releaseWhoami := make(chan struct{})
	var closeWhoamiStarted sync.Once

	var mu sync.Mutex
	whoamiCalls := 0

	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "whoami") {
			mu.Lock()
			whoamiCalls++
			mu.Unlock()
			closeWhoamiStarted.Do(func() { close(whoamiStarted) })
			<-releaseWhoami
			return []byte(`{"user_uuid":"user"}`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", argsStr)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := b.CheckAuth(context.Background()); err != nil {
				errs <- err
			}
		}()
	}

	close(start)
	select {
	case <-whoamiStarted:
	case <-time.After(time.Second):
		t.Fatal("op whoami did not start")
	}

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	gotWhoamiWhileBlocked := whoamiCalls
	mu.Unlock()
	if gotWhoamiWhileBlocked != 1 {
		t.Fatalf("whoami calls while auth blocked = %d, want 1", gotWhoamiWhileBlocked)
	}

	close(releaseWhoami)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if whoamiCalls != 1 {
		t.Fatalf("whoami calls = %d, want 1", whoamiCalls)
	}
}

func TestOnePasswordBackend_CheckAuth_DoesNotCacheFailure(t *testing.T) {
	b := &OnePasswordBackend{
		binary:       "op.exe",
		vault:        "test-vault",
		authCacheTTL: time.Minute,
	}

	var mu sync.Mutex
	whoamiCalls := 0
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "whoami") {
			mu.Lock()
			whoamiCalls++
			call := whoamiCalls
			mu.Unlock()
			if call == 1 {
				return nil, fmt.Errorf("op command failed: locked")
			}
			return []byte(`{"user_uuid":"user"}`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", argsStr)
	}

	if err := b.CheckAuth(context.Background()); err == nil {
		t.Fatal("first CheckAuth succeeded, want auth failure")
	}
	if err := b.CheckAuth(context.Background()); err != nil {
		t.Fatalf("second CheckAuth failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if whoamiCalls != 2 {
		t.Fatalf("whoami calls = %d, want 2", whoamiCalls)
	}
}

func TestOnePasswordBackend_runCmd_Stdin(t *testing.T) {
	backend, err := NewOnePasswordBackend()
	if err != nil {
		t.Fatalf("failed to create backend: %v", err)
	}

	ctx := context.Background()
	// Run 'cat' to read from stdin and output it.
	out, err := backend.runCmd(ctx, `{"fields":[]}`, "cat")
	if err != nil {
		t.Fatalf("runCmd with cat failed: %v", err)
	}

	expected := `{"fields":[]}`
	if string(out) != expected {
		t.Errorf("expected stdin to be %q, got %q", expected, string(out))
	}
}

func TestOnePasswordBackend_RunOP_DoesNotLogSecrets(t *testing.T) {
	var logs bytes.Buffer
	origOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() {
		log.SetOutput(origOutput)
	})

	b := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "test-vault",
		runCmd: func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
			return []byte(`{"id":"id1"}`), nil
		},
	}

	_, err := b.runOP(context.Background(), "item", "create", "password=super-secret-token", "attributes[text]=service=gh%3Agithub.com")
	if err != nil {
		t.Fatalf("runOP failed: %v", err)
	}

	got := logs.String()
	if strings.Contains(got, "super-secret-token") || strings.Contains(got, "password=") || strings.Contains(got, "attributes[text]") {
		t.Fatalf("runOP logged sensitive command arguments: %s", got)
	}
}

func TestOnePasswordBackend_RunOP_DoesNotReturnStderr(t *testing.T) {
	b := newAuthenticatedTestOnePasswordBackend()

	operationCalls := 0
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "whoami") {
			t.Fatalf("test backend should already be authenticated, got auth command: %s", argsStr)
		}
		if !strings.Contains(argsStr, "item create") {
			return nil, fmt.Errorf("unexpected command: %s", argsStr)
		}
		operationCalls++
		cmd := exec.CommandContext(ctx, "sh", "-c", "echo super-secret-token >&2; exit 42")
		return cmd.Output()
	}

	_, err := b.runOP(context.Background(), "item", "create")
	if err == nil {
		t.Fatal("expected runOP to fail")
	}
	if operationCalls != 1 {
		t.Fatalf("operation calls = %d, want 1", operationCalls)
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("runOP returned stderr containing secret: %v", err)
	}
}

func TestOnePasswordBackend_Get_URLQuery(t *testing.T) {
	b := newAuthenticatedTestOnePasswordBackend()

	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")

		if !strings.Contains(argsStr, "item get test-id") {
			return nil, fmt.Errorf("unexpected command: %s", argsStr)
		}

		attrsQuery := "app=vscode&env=dev"
		resp := opItem{
			ID:    "test-id",
			Title: "My Saved Secret",
			Fields: []opItemField{
				{ID: "username", Type: "STRING", Value: "alice"},
				{ID: "password", Type: "CONCEALED", Value: "supersecure"},
				{ID: "random-id-123", Label: "attributes", Type: "STRING", Value: attrsQuery},
			},
		}
		return json.Marshal(resp)
	}

	item, err := b.Get(context.Background(), "test-id")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if item.ID != "test-id" {
		t.Errorf("expected test-id, got %s", item.ID)
	}
	if item.Label != "My Saved Secret" {
		t.Errorf("expected My Saved Secret, got %s", item.Label)
	}
	if string(item.Secret) != "supersecure" {
		t.Errorf("expected supersecure, got %s", string(item.Secret))
	}
	if item.Attributes["username"] != "alice" || item.Attributes["app"] != "vscode" || item.Attributes["env"] != "dev" {
		t.Errorf("unexpected attributes: %+v", item.Attributes)
	}
}

func TestOnePasswordBackend_LoadMetadataUsesListTagsWithoutItemGet(t *testing.T) {
	b := newAuthenticatedTestOnePasswordBackend()

	calls := 0
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		calls++
		argsStr := strings.Join(args, " ")
		if !strings.Contains(argsStr, "item list") {
			return nil, fmt.Errorf("unexpected command: %s", argsStr)
		}
		return json.Marshal([]opListItem{{
			ID:    "id1",
			Title: "My Saved Secret",
			Tags: []string{
				"wsl-keyring",
				"wsl-keyring-meta-v1",
				"wsl-keyring-attr:c2VydmljZQ=Z2g6Z2l0aHViLmNvbQ",
				"wsl-keyring-attr:dXNlcm5hbWU=YWxpY2U",
			},
		}})
	}

	items, err := b.LoadMetadata(context.Background())
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("op calls = %d, want 1", calls)
	}
	if len(items) != 1 {
		t.Fatalf("LoadMetadata returned %d items, want 1", len(items))
	}
	if items[0].ID != "id1" || items[0].Label != "My Saved Secret" || items[0].Secret != nil {
		t.Fatalf("unexpected metadata item: %+v", items[0])
	}
	if items[0].Attributes["service"] != "gh:github.com" || items[0].Attributes["username"] != "alice" {
		t.Fatalf("unexpected attributes: %+v", items[0].Attributes)
	}
}

func TestOnePasswordBackend_LoadMetadataMigratesLegacyItemTags(t *testing.T) {
	b := newAuthenticatedTestOnePasswordBackend()

	var commands []string
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		commands = append(commands, argsStr)
		switch {
		case strings.Contains(argsStr, "item list"):
			return json.Marshal([]opListItem{{
				ID:    "id1",
				Title: "legacy title from list",
				Tags:  []string{"wsl-keyring", "keep-me"},
			}})
		case strings.Contains(argsStr, "item get id1"):
			return json.Marshal(opItem{
				ID:    "id1",
				Title: "legacy title from get",
				Fields: []opItemField{
					{ID: "username", Type: "STRING", Value: "alice"},
					{ID: "attributes", Label: "attributes", Type: "STRING", Value: "service=gh%3Agithub.com&username=alice"},
				},
			})
		case strings.Contains(argsStr, "item edit id1"):
			if stdin != "" {
				t.Fatalf("metadata migration must not pass item template stdin, got %q", stdin)
			}
			if !strings.Contains(argsStr, "keep-me") || !strings.Contains(argsStr, "wsl-keyring") || !strings.Contains(argsStr, "wsl-keyring-meta-v1") {
				t.Fatalf("migration did not preserve existing tags or add metadata marker: %s", argsStr)
			}
			if !strings.Contains(argsStr, "wsl-keyring-attr:c2VydmljZQ=Z2g6Z2l0aHViLmNvbQ") || !strings.Contains(argsStr, "wsl-keyring-attr:dXNlcm5hbWU=YWxpY2U") {
				t.Fatalf("migration did not include attribute tags: %s", argsStr)
			}
			return []byte(`{"id":"id1"}`), nil
		default:
			return nil, fmt.Errorf("unexpected command: %s", argsStr)
		}
	}

	items, err := b.LoadMetadata(context.Background())
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("LoadMetadata returned %d items, want 1", len(items))
	}
	if items[0].ID != "id1" || items[0].Label != "legacy title from get" || items[0].Attributes["service"] != "gh:github.com" {
		t.Fatalf("unexpected migrated metadata item: %+v", items[0])
	}
	if len(commands) != 3 {
		t.Fatalf("commands = %+v, want list, get, edit", commands)
	}
}

func TestOnePasswordBackend_LoadMetadataIgnoresLegacyMigrationFailure(t *testing.T) {
	b := newAuthenticatedTestOnePasswordBackend()

	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		switch {
		case strings.Contains(argsStr, "item list"):
			return json.Marshal([]opListItem{{ID: "id1", Title: "legacy"}})
		case strings.Contains(argsStr, "item get id1"):
			return json.Marshal(opItem{
				ID:    "id1",
				Title: "legacy",
				Fields: []opItemField{
					{ID: "username", Type: "STRING", Value: "alice"},
					{ID: "attributes", Label: "attributes", Type: "STRING", Value: "service=github&username=alice"},
				},
			})
		case strings.Contains(argsStr, "item edit id1"):
			return nil, fmt.Errorf("edit failed")
		default:
			return nil, fmt.Errorf("unexpected command: %s", argsStr)
		}
	}

	items, err := b.LoadMetadata(context.Background())
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}
	if len(items) != 1 || items[0].Attributes["service"] != "github" {
		t.Fatalf("unexpected metadata items: %+v", items)
	}
}

func TestOnePasswordBackend_Search(t *testing.T) {
	b := newAuthenticatedTestOnePasswordBackend()

	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "item list") {
			list := []opListItem{
				{ID: "id1", Title: "Password for 'alice' on 'gh:github.com'"},
				{ID: "id2", Title: "Password for 'bob' on 'gitlab.com'"},
			}
			return json.Marshal(list)
		} else if strings.Contains(argsStr, "item get id1") {
			resp := opItem{
				ID:    "id1",
				Title: "Password for 'alice' on 'gh:github.com'",
				Fields: []opItemField{
					{ID: "username", Type: "STRING", Value: "alice"},
					{ID: "password", Type: "CONCEALED", Value: "token123"},
					{ID: "attributes", Label: "attributes", Type: "STRING", Value: "service=gh%3Agithub.com&username=alice"},
				},
			}
			return json.Marshal(resp)
		} else if strings.Contains(argsStr, "item get id2") {
			resp := opItem{
				ID:    "id2",
				Title: "Password for 'bob' on 'gitlab.com'",
				Fields: []opItemField{
					{ID: "username", Type: "STRING", Value: "bob"},
					{ID: "password", Type: "CONCEALED", Value: "token456"},
					{ID: "attributes", Label: "attributes", Type: "STRING", Value: "service=gitlab.com&username=bob"},
				},
			}
			return json.Marshal(resp)
		}
		return nil, fmt.Errorf("unexpected command: %s", argsStr)
	}

	// Search for username=alice, service=gh:github.com
	matched, err := b.Search(context.Background(), map[string]string{
		"username": "alice",
		"service":  "gh:github.com",
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if len(matched) != 1 {
		t.Errorf("expected 1 match, got %d", len(matched))
	} else {
		item := matched[0]
		if item.ID != "id1" || len(item.Secret) != 0 {
			t.Errorf("unexpected item matched: %+v", item)
		}
	}
}

func TestOnePasswordBackend_Search_EmptyValueRequiresExactMatch(t *testing.T) {
	b := newAuthenticatedTestOnePasswordBackend()

	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "item list") {
			list := []opListItem{{ID: "id1", Title: "Password for 'alice' on 'gh:github.com'"}}
			return json.Marshal(list)
		} else if strings.Contains(argsStr, "item get id1") {
			resp := opItem{
				ID:    "id1",
				Title: "Password for 'alice' on 'gh:github.com'",
				Fields: []opItemField{
					{ID: "username", Type: "STRING", Value: "alice"},
					{ID: "password", Type: "CONCEALED", Value: "token123"},
					{ID: "attributes", Label: "attributes", Type: "STRING", Value: "service=gh%3Agithub.com&username=alice"},
				},
			}
			return json.Marshal(resp)
		}
		return nil, fmt.Errorf("unexpected command: %s", argsStr)
	}

	matched, err := b.Search(context.Background(), map[string]string{
		"username": "",
		"service":  "gh:github.com",
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(matched) != 0 {
		t.Fatalf("expected no matches for empty username, got %d", len(matched))
	}
}

func TestOnePasswordBackend_List_LoadsMetadata(t *testing.T) {
	b := newAuthenticatedTestOnePasswordBackend()

	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		switch {
		case strings.Contains(argsStr, "item list"):
			return json.Marshal([]opListItem{{ID: "id1", Title: "ignored"}})
		case strings.Contains(argsStr, "item get id1"):
			return json.Marshal(opItem{
				ID:    "id1",
				Title: "My Saved Secret",
				Fields: []opItemField{
					{ID: "username", Type: "STRING", Value: "alice"},
					{ID: "attributes", Label: "attributes", Type: "STRING", Value: "service=github&username=alice"},
				},
			})
		default:
			return nil, fmt.Errorf("unexpected command: %s", argsStr)
		}
	}

	items, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List returned %d items, want 1", len(items))
	}
	if items[0].ID != "id1" || items[0].Label != "My Saved Secret" || items[0].Attributes["service"] != "github" || items[0].Secret != nil {
		t.Fatalf("unexpected item: %+v", items[0])
	}
}

func TestOnePasswordBackend_Delete_RunsOPDelete(t *testing.T) {
	b := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "test-vault",
	}
	var gotArgs string
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		gotArgs = strings.Join(args, " ")
		return []byte(`{}`), nil
	}

	if err := b.Delete(context.Background(), "id1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if !strings.Contains(gotArgs, "item delete id1") || !strings.Contains(gotArgs, "--vault test-vault") {
		t.Fatalf("unexpected delete args: %s", gotArgs)
	}
}

func TestOnePasswordBackend_Save_CreateUpdatesExistingMatchingItem(t *testing.T) {
	b := newAuthenticatedTestOnePasswordBackend()

	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		switch {
		case strings.Contains(argsStr, "item list"):
			return json.Marshal([]opListItem{{ID: "existing-id", Title: "existing"}})
		case strings.Contains(argsStr, "item get existing-id"):
			return json.Marshal(opItem{
				ID:    "existing-id",
				Title: "existing",
				Fields: []opItemField{
					{ID: "username", Type: "STRING", Value: "alice"},
					{ID: "attributes", Label: "attributes", Type: "STRING", Value: "service=github&username=alice"},
				},
			})
		case strings.Contains(argsStr, "item edit existing-id"):
			var template opItem
			if err := json.Unmarshal([]byte(stdin), &template); err != nil {
				t.Fatalf("failed to parse stdin template: %v", err)
			}
			if template.Title != "updated" {
				t.Fatalf("template title = %q", template.Title)
			}
			return []byte(`{"id":"existing-id","title":"updated"}`), nil
		default:
			return nil, fmt.Errorf("unexpected command: %s", argsStr)
		}
	}

	item := &SecretItem{
		Label:      "updated",
		Attributes: map[string]string{"service": "github", "username": "alice"},
		Secret:     []byte("new-secret"),
	}
	if err := b.Save(context.Background(), item); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if item.ID != "existing-id" {
		t.Fatalf("item ID = %q, want existing-id", item.ID)
	}
}

func TestOnePasswordBackend_Save_EditPersistsInBackground(t *testing.T) {
	b := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "test-vault",
	}

	started := make(chan struct{})
	release := make(chan struct{})
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "item edit test-id") {
			close(started)
			if strings.Contains(argsStr, "password=new-secret") {
				t.Fatalf("secret fields must be passed via stdin, got args: %s", argsStr)
			}
			var template opItem
			if err := json.Unmarshal([]byte(stdin), &template); err != nil {
				t.Fatalf("failed to parse stdin template: %v", err)
			}
			if template.Category != "LOGIN" {
				t.Fatalf("expected LOGIN category, got %q", template.Category)
			}
			fields := map[string]string{}
			fieldMeta := map[string]opItemField{}
			for _, field := range template.Fields {
				fields[field.ID] = field.Value
				fieldMeta[field.ID] = field
			}
			if fields["username"] != "bob" || fields["password"] != "new-secret" {
				t.Fatalf("unexpected template fields: %+v", fields)
			}
			if fieldMeta["password"].Purpose != "PASSWORD" || fieldMeta["password"].Type != "CONCEALED" {
				t.Fatalf("unexpected password field metadata: %+v", fieldMeta["password"])
			}
			<-release
			return []byte(`{"id":"test-id","title":"My Label"}`), nil
		}
		if strings.Contains(argsStr, "whoami") {
			return []byte(`{"user_uuid":"user"}`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", argsStr)
	}
	backend := NewCachedBackend(b, BackendOptions{
		CacheSecrets:        true,
		CacheMetadata:       true,
		AsyncSave:           true,
		AuthCheckMinSpacing: time.Hour,
	})

	item := &SecretItem{
		ID:         "test-id",
		Label:      "My Label",
		Attributes: map[string]string{"username": "bob"},
		Secret:     []byte("new-secret"),
	}

	saveDone := make(chan error, 1)
	go func() {
		saveDone <- backend.Save(context.Background(), item)
	}()

	select {
	case <-started:
		select {
		case err := <-saveDone:
			if err != nil {
				t.Fatalf("Save failed: %v", err)
			}
		case <-time.After(50 * time.Millisecond):
			close(release)
			t.Fatal("Save blocked waiting for op edit")
		}
	case err := <-saveDone:
		if err != nil {
			t.Fatalf("Save failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("op edit did not start")
	}

	got, err := backend.Get(context.Background(), "test-id")
	if err != nil {
		t.Fatalf("Get from cache after Save failed: %v", err)
	}
	if string(got.Secret) != "new-secret" {
		t.Fatalf("cached secret = %q", string(got.Secret))
	}
	close(release)
}
