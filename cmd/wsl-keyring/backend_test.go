package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"testing"
)

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

func TestOnePasswordBackend_Save_Create(t *testing.T) {
	b := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "test-vault",
	}

	called := false
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		called = true
		if name != "op.exe" {
			t.Errorf("expected op.exe, got %s", name)
		}

		// Check basic arguments
		argsStr := strings.Join(args, " ")

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

		resp := opItem{
			ID:    "generated-uuid-5555",
			Title: "My Label",
		}
		return json.Marshal(resp)
	}

	item := &SecretItem{
		Label: "My Label",
		Attributes: map[string]string{
			"username": "bob",
			"app":      "vscode",
		},
		Secret: []byte("secret123"),
	}

	err := b.Save(context.Background(), item)
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if !called {
		t.Error("mock runCmd was not called")
	}
	if item.ID != "generated-uuid-5555" {
		t.Errorf("expected ID to be set to generated-uuid-5555, got %s", item.ID)
	}
}

func TestOnePasswordBackend_Get(t *testing.T) {
	b := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "test-vault",
	}

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
	b := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "test-vault",
		runCmd: func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, "sh", "-c", "echo super-secret-token >&2; exit 42")
			return cmd.Output()
		},
	}

	_, err := b.runOP(context.Background(), "item", "create")
	if err == nil {
		t.Fatal("expected runOP to fail")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("runOP returned stderr containing secret: %v", err)
	}
}

func TestOnePasswordBackend_Get_URLQuery(t *testing.T) {
	b := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "test-vault",
	}

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

func TestOnePasswordBackend_Search(t *testing.T) {
	b := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "test-vault",
	}

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
	b := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "test-vault",
	}

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

func TestOnePasswordBackend_Save_Edit_Sync(t *testing.T) {
	b := &OnePasswordBackend{
		binary: "op.exe",
		vault:  "test-vault",
	}

	editCompleted := false
	b.runCmd = func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
		argsStr := strings.Join(args, " ")
		if strings.Contains(argsStr, "item edit test-id") {
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
			editCompleted = true
			return []byte(`{"id":"test-id","title":"My Label"}`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", argsStr)
	}

	item := &SecretItem{
		ID:         "test-id",
		Label:      "My Label",
		Attributes: map[string]string{"username": "bob"},
		Secret:     []byte("new-secret"),
	}

	err := b.Save(context.Background(), item)
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if !editCompleted {
		t.Fatal("Save returned before item edit completed")
	}
}
