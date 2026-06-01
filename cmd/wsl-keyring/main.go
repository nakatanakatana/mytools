package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/caarlos0/env/v11"
	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

type Config struct {
	UseInMemory bool `env:"USE_IN_MEMORY"`
	Init        bool `env:"INIT"`
}

func main() {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		log.Fatalf("Failed to parse environment variables: %v", err)
	}

	if cfg.Init {
		initServiceFile()
		return
	}

	// Connect to Session Bus
	conn, err := dbus.SessionBus()
	if err != nil {
		log.Fatalf("Failed to connect to session bus: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	var backend StorageBackend
	if cfg.UseInMemory {
		backend = NewInMemoryBackend()
	} else {
		opBackend, err := NewOnePasswordBackend()
		if err != nil {
			log.Fatalf("Failed to initialize 1Password backend: %v", err)
		}
		backend = NewCachedBackend(opBackend, BackendOptions{
			CacheSecrets:  true,
			CacheMetadata: true,
			AsyncSave:     true,
		})
	}

	// Create service object
	service := NewServiceObject(conn, backend)

	// Export Service
	err = conn.Export(service, ServicePath, ServiceInterface)
	if err != nil {
		log.Fatalf("Failed to export service: %v", err)
	}
	err = conn.Export(service, ServicePath, PropertiesInterface)
	if err != nil {
		log.Fatalf("Failed to export service properties: %v", err)
	}
	if err := conn.Export(introspect.Introspectable(serviceIntrospectXML), ServicePath, "org.freedesktop.DBus.Introspectable"); err != nil {
		log.Fatalf("Failed to export service introspection: %v", err)
	}

	// Export Default Collection
	collection := NewCollectionObject(conn, backend, service)
	for _, colPath := range []dbus.ObjectPath{
		CollectionPath,
		"/org/freedesktop/secrets/aliases/default",
	} {
		err = conn.Export(collection, colPath, CollectionInterface)
		if err != nil {
			log.Fatalf("Failed to export collection at %s: %v", colPath, err)
		}
		err = conn.Export(collection, colPath, PropertiesInterface)
		if err != nil {
			log.Fatalf("Failed to export collection properties at %s: %v", colPath, err)
		}
		if err := conn.Export(introspect.Introspectable(collectionIntrospectXML), colPath, "org.freedesktop.DBus.Introspectable"); err != nil {
			log.Fatalf("Failed to export collection introspection at %s: %v", colPath, err)
		}
	}

	// Pre-load existing items and export them asynchronously to avoid blocking service startup
	go func() {
		items, err := backend.List(context.Background())
		if err != nil {
			return
		}
		for _, item := range items {
			if item != nil {
				_ = service.ExportItem(item)
			}
		}
	}()

	// Request org.freedesktop.secrets name
	reply, err := conn.RequestName("org.freedesktop.secrets", dbus.NameFlagReplaceExisting)
	if err != nil {
		log.Fatalf("Failed to request D-Bus name: %v", err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		log.Fatalf("Name org.freedesktop.secrets already taken")
	}

	// Listen for termination signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
}

func initServiceFile() {
	execPath, err := os.Executable()
	if err != nil {
		log.Fatalf("Failed to get executable path: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Failed to get user home directory: %v", err)
	}

	dir := filepath.Join(home, ".local", "share", "dbus-1", "services")
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Fatalf("Failed to create directory %s: %v", dir, err)
	}

	envs := buildEnvs(os.Getenv, exec.LookPath)

	execCmd := execPath
	if len(envs) > 0 {
		execCmd = fmt.Sprintf("/usr/bin/env %s %s", strings.Join(envs, " "), execPath)
	}

	filePath := filepath.Join(dir, "org.freedesktop.secrets.service")
	content := fmt.Sprintf(`[D-BUS Service]
Name=org.freedesktop.secrets
Exec=%s
`, execCmd)

	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		log.Fatalf("Failed to write service file: %v", err)
	}

	log.Printf("Successfully initialized D-Bus activation service file at:\n  %s\nPointing to:\n  %s\n", filePath, execCmd)
}

func buildEnvs(getenv func(string) string, lookPath func(string) (string, error)) []string {
	var envs []string
	if v := getenv("OP_VAULT"); v != "" {
		envs = append(envs, fmt.Sprintf("OP_VAULT=%q", v))
	}
	if v := getenv("OP_BINARY"); v != "" {
		resolved := v
		if abs, err := lookPath(v); err == nil {
			if absPath, err := filepath.Abs(abs); err == nil {
				resolved = absPath
			} else {
				resolved = abs
			}
		}
		envs = append(envs, fmt.Sprintf("OP_BINARY=%q", resolved))
	}
	if v := getenv("USE_IN_MEMORY"); v != "" {
		envs = append(envs, fmt.Sprintf("USE_IN_MEMORY=%q", v))
	}
	return envs
}
