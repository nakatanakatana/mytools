package main

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestServerAddress(t *testing.T) {
	cfg := Config{Host: "127.0.0.1", Port: "3334", ManagementHost: "127.0.0.2", ManagementPort: "3335"}
	if got := ServerAddress(cfg); got != "127.0.0.1:3334" {
		t.Fatalf("ServerAddress() = %q", got)
	}
	if got := ManagementServerAddress(cfg); got != "127.0.0.2:3335" {
		t.Fatalf("ManagementServerAddress() = %q", got)
	}
	cfg.Host, cfg.ManagementHost = "::", "::1"
	if got := ServerAddress(cfg); got != "[::]:3334" {
		t.Fatalf("IPv6 ServerAddress() = %q", got)
	}
	if got := ManagementServerAddress(cfg); got != "[::1]:3335" {
		t.Fatalf("IPv6 ManagementServerAddress() = %q", got)
	}
	cfg.Host, cfg.ManagementHost = "[::]", "[::1]"
	if got := ServerAddress(cfg); got != "[::]:3334" {
		t.Fatalf("bracketed IPv6 ServerAddress() = %q", got)
	}
	if got := ManagementServerAddress(cfg); got != "[::1]:3335" {
		t.Fatalf("bracketed IPv6 ManagementServerAddress() = %q", got)
	}
	cfg.Host, cfg.ManagementHost = "fe80::1%test-zone", "fe80::2%test-zone"
	if got := ServerAddress(cfg); got != "[fe80::1%test-zone]:3334" {
		t.Fatalf("scoped IPv6 ServerAddress() = %q", got)
	}
	if got := ManagementServerAddress(cfg); got != "[fe80::2%test-zone]:3335" {
		t.Fatalf("scoped IPv6 ManagementServerAddress() = %q", got)
	}
}

func TestRunShutsDownServerBeforeClosingResources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wantCloseErr := errors.New("close store")
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	server := newFakeRelayServer()
	var eventsMu sync.Mutex
	var events []string
	server.record = func(event string) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, event)
	}
	closeCalls := 0
	deps := runtimeDependencies{
		newRelay: func(context.Context, Config) (RelayResources, error) {
			return RelayResources{Handler: handler, Close: func() error {
				eventsMu.Lock()
				defer eventsMu.Unlock()
				closeCalls++
				events = append(events, "resources-close")
				return wantCloseErr
			}}, nil
		},
		newServer: func(addr string, got http.Handler) relayServer {
			if addr != "127.0.0.1:3334" {
				t.Errorf("server addr = %q", addr)
			}
			if reflect.ValueOf(got).Pointer() != reflect.ValueOf(handler).Pointer() {
				t.Error("runtime did not serve RelayResources.Handler")
			}
			return server
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, Config{Host: "127.0.0.1", Port: "3334"}, deps)
	}()
	<-server.started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, wantCloseErr) {
			t.Fatalf("run() error = %v, want close error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not stop after context cancellation")
	}
	if closeCalls != 1 {
		t.Fatalf("resource close calls = %d, want 1", closeCalls)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if want := []string{"serve", "shutdown", "serve-drained", "resources-close"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle events = %v, want %v", events, want)
	}
}

func TestRunListenErrorClosesResources(t *testing.T) {
	wantListenErr := errors.New("listen failed")
	closeCalls := 0
	deps := runtimeDependencies{
		newRelay: func(context.Context, Config) (RelayResources, error) {
			return RelayResources{Handler: http.NotFoundHandler(), Close: func() error {
				closeCalls++
				return nil
			}}, nil
		},
		newServer: func(string, http.Handler) relayServer {
			return immediateErrorServer{err: wantListenErr}
		},
	}

	err := runWithDependencies(context.Background(), Config{}, deps)
	if !errors.Is(err, wantListenErr) {
		t.Fatalf("run() error = %v, want listen error", err)
	}
	if closeCalls != 1 {
		t.Fatalf("resource close calls = %d, want 1", closeCalls)
	}
}

func TestRunServesProtocolAndManagementOnDifferentAddresses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	protocol := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	management := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	servers := []*fakeRelayServer{newFakeRelayServer(), newFakeRelayServer()}
	for _, server := range servers {
		server.record = func(string) {}
	}
	type binding struct {
		address string
		handler http.Handler
	}
	var bindings []binding
	deps := runtimeDependencies{
		newRelay: func(context.Context, Config) (RelayResources, error) {
			return RelayResources{ProtocolHandler: protocol, ManagementHandler: management, Close: func() error { return nil }}, nil
		},
		newServer: func(address string, handler http.Handler) relayServer {
			bindings = append(bindings, binding{address: address, handler: handler})
			return servers[len(bindings)-1]
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, Config{Host: "127.0.0.1", Port: "8080", ManagementHost: "127.0.0.1", ManagementPort: "8081"}, deps)
	}()
	for _, server := range servers {
		select {
		case <-server.started:
		case <-time.After(time.Second):
			t.Fatal("listener did not start")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 || bindings[0].address != "127.0.0.1:8080" || bindings[1].address != "127.0.0.1:8081" {
		t.Fatalf("bindings = %#v", bindings)
	}
	if reflect.ValueOf(bindings[0].handler).Pointer() != reflect.ValueOf(protocol).Pointer() || reflect.ValueOf(bindings[1].handler).Pointer() != reflect.ValueOf(management).Pointer() {
		t.Fatal("protocol and management handlers were not isolated")
	}
}

type fakeRelayServer struct {
	started  chan struct{}
	stopped  chan struct{}
	record   func(string)
	stopOnce sync.Once
}

func newFakeRelayServer() *fakeRelayServer {
	return &fakeRelayServer{started: make(chan struct{}), stopped: make(chan struct{})}
}

func (s *fakeRelayServer) ListenAndServe() error {
	s.record("serve")
	close(s.started)
	<-s.stopped
	s.record("serve-drained")
	return http.ErrServerClosed
}

func (s *fakeRelayServer) Shutdown(context.Context) error {
	s.record("shutdown")
	s.stopOnce.Do(func() { close(s.stopped) })
	return nil
}

func (s *fakeRelayServer) Close() error {
	s.stopOnce.Do(func() { close(s.stopped) })
	return nil
}

type immediateErrorServer struct{ err error }

func (s immediateErrorServer) ListenAndServe() error        { return s.err }
func (immediateErrorServer) Shutdown(context.Context) error { return nil }
func (immediateErrorServer) Close() error                   { return nil }
