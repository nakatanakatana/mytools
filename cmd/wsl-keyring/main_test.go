package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
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

func TestBuildServiceFileContent(t *testing.T) {
	got := buildServiceFileContent(`/usr/bin/env OP_VAULT="my-vault" /tmp/wsl-keyring`)
	want := `[D-BUS Service]
Name=org.freedesktop.secrets
Exec=/usr/bin/env OP_VAULT="my-vault" /tmp/wsl-keyring
`
	if got != want {
		t.Fatalf("service file content = %q, want %q", got, want)
	}
}

func TestConfigCacheBackendOptionsUsesEnvDurations(t *testing.T) {
	t.Setenv("WSL_KEYRING_SECRET_CACHE_TTL", "15m")
	t.Setenv("WSL_KEYRING_AUTH_CHECK_MIN_SPACING", "30s")
	t.Setenv("WSL_KEYRING_AUTH_CHECK_TIMEOUT", "750ms")

	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("env.Parse failed: %v", err)
	}

	opts := cfg.CacheBackendOptions()
	if !opts.CacheSecrets || !opts.CacheMetadata || !opts.AsyncSave {
		t.Fatalf("cache options should enable production caches: %+v", opts)
	}
	if got, want := opts.SecretCacheTTL, 15*time.Minute; got != want {
		t.Fatalf("SecretCacheTTL = %s, want %s", got, want)
	}
	if got, want := opts.AuthCheckMinSpacing, 30*time.Second; got != want {
		t.Fatalf("AuthCheckMinSpacing = %s, want %s", got, want)
	}
	if got, want := opts.AuthCheckTimeout, 750*time.Millisecond; got != want {
		t.Fatalf("AuthCheckTimeout = %s, want %s", got, want)
	}
}

func TestWriteServiceFile(t *testing.T) {
	home := t.TempDir()
	getenv := func(key string) string {
		if key == "OP_VAULT" {
			return "my-vault"
		}
		return ""
	}
	lookPath := func(s string) (string, error) {
		return "", errors.New("not found")
	}

	filePath, execCmd, err := writeServiceFile("/tmp/wsl-keyring", home, getenv, lookPath)
	if err != nil {
		t.Fatalf("writeServiceFile failed: %v", err)
	}
	wantPath := filepath.Join(home, ".local", "share", "dbus-1", "services", "org.freedesktop.secrets.service")
	if filePath != wantPath {
		t.Fatalf("filePath = %s, want %s", filePath, wantPath)
	}
	wantExec := `/usr/bin/env OP_VAULT="my-vault" /tmp/wsl-keyring`
	if execCmd != wantExec {
		t.Fatalf("execCmd = %q, want %q", execCmd, wantExec)
	}
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read service file: %v", err)
	}
	if string(content) != buildServiceFileContent(wantExec) {
		t.Fatalf("service file content = %q", content)
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

func TestServiceReadAliasSignatureMatchesSecretServiceSpec(t *testing.T) {
	method, ok := reflect.TypeOf(&ServiceObject{}).MethodByName("ReadAlias")
	if !ok {
		t.Fatal("ServiceObject.ReadAlias method not found")
	}

	if got, want := method.Type.NumIn(), 2; got != want {
		t.Fatalf("ReadAlias NumIn = %d, want %d", got, want)
	}
	if got, want := method.Type.In(1), reflect.TypeOf(""); got != want {
		t.Fatalf("ReadAlias input = %v, want %v", got, want)
	}
	if got, want := method.Type.NumOut(), 2; got != want {
		t.Fatalf("ReadAlias NumOut = %d, want %d", got, want)
	}
	if got, want := method.Type.Out(0), reflect.TypeOf(dbus.ObjectPath("")); got != want {
		t.Fatalf("ReadAlias first output = %v, want %v", got, want)
	}
	if got, want := method.Type.Out(1), reflect.TypeOf((*dbus.Error)(nil)); got != want {
		t.Fatalf("ReadAlias second output = %v, want %v", got, want)
	}
}

func TestServiceIntrospectionIncludesReadAlias(t *testing.T) {
	if !strings.Contains(serviceIntrospectXML, `<method name="ReadAlias">`) {
		t.Fatal("service introspection XML does not include ReadAlias")
	}
}
