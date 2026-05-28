package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestBuildEnvs(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		lookPath func(string) (string, error)
		want     []string
	}{
		{
			name: "all empty",
			env:  map[string]string{},
			lookPath: func(s string) (string, error) {
				return s, errors.New("not found")
			},
			want: nil,
		},
		{
			name: "OP_VAULT and USE_IN_MEMORY set",
			env: map[string]string{
				"OP_VAULT":      "my-vault",
				"USE_IN_MEMORY": "true",
			},
			lookPath: func(s string) (string, error) {
				return s, errors.New("not found")
			},
			want: []string{
				`OP_VAULT="my-vault"`,
				`USE_IN_MEMORY="true"`,
			},
		},
		{
			name: "OP_BINARY set as command name and resolved successfully",
			env: map[string]string{
				"OP_BINARY": "op",
			},
			lookPath: func(s string) (string, error) {
				if s == "op" {
					return "/usr/local/bin/op", nil
				}
				return s, errors.New("not found")
			},
			want: []string{
				`OP_BINARY="/usr/local/bin/op"`,
			},
		},
		{
			name: "OP_BINARY set as command name but not found in path",
			env: map[string]string{
				"OP_BINARY": "op-missing",
			},
			lookPath: func(s string) (string, error) {
				return "", errors.New("not found")
			},
			want: []string{
				`OP_BINARY="op-missing"`,
			},
		},
		{
			name: "OP_BINARY is already absolute path",
			env: map[string]string{
				"OP_BINARY": "/opt/bin/op",
			},
			lookPath: func(s string) (string, error) {
				return "/opt/bin/op", nil
			},
			want: []string{
				`OP_BINARY="/opt/bin/op"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string {
				return tt.env[key]
			}
			got := buildEnvs(getenv, tt.lookPath)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildEnvs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCollectionSearchItemsSignatureMatchesSecretServiceSpec(t *testing.T) {
	method, ok := reflect.TypeOf(&CollectionObject{}).MethodByName("SearchItems")
	if !ok {
		t.Fatal("CollectionObject.SearchItems method not found")
	}

	if got, want := method.Type.NumIn(), 2; got != want {
		t.Fatalf("SearchItems NumIn = %d, want %d", got, want)
	}
	if got, want := method.Type.In(1), reflect.TypeOf(map[string]string{}); got != want {
		t.Fatalf("SearchItems input = %v, want %v", got, want)
	}
	if got, want := method.Type.NumOut(), 2; got != want {
		t.Fatalf("SearchItems NumOut = %d, want %d", got, want)
	}
	if got, want := method.Type.Out(0), reflect.TypeOf([]dbus.ObjectPath{}); got != want {
		t.Fatalf("SearchItems first output = %v, want %v", got, want)
	}
	if got, want := method.Type.Out(1), reflect.TypeOf((*dbus.Error)(nil)); got != want {
		t.Fatalf("SearchItems second output = %v, want %v", got, want)
	}
}
