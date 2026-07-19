package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/bluesky"
)

type reconciliationSource struct{}

func (reconciliationSource) Timeline(context.Context, string, int) (bluesky.Page, error) {
	return bluesky.Page{}, nil
}
func (reconciliationSource) Follows(context.Context) ([]bluesky.Actor, error) { return nil, nil }
func (reconciliationSource) List(context.Context, string) (bluesky.List, error) {
	return bluesky.List{}, nil
}
func (reconciliationSource) Profile(_ context.Context, did string) (bluesky.Profile, error) {
	return bluesky.Profile{DID: did, Handle: "handle", DisplayName: did}, nil
}

func TestServerAddress(t *testing.T) {
	if got := ServerAddress(Config{Shared: SharedConfig{Host: "127.0.0.1", Port: "4321"}}); got != "127.0.0.1:4321" {
		t.Fatalf("ServerAddress() = %q", got)
	}
}

func TestRunStopsResourcesWhenContextEnds(t *testing.T) {
	oldNewRuntimeResources := newRuntimeResources
	t.Cleanup(func() { newRuntimeResources = oldNewRuntimeResources })
	newRuntimeResources = func(Config) (runtimeResources, error) {
		return runtimeResources{httpServer: &http.Server{}, jetstream: completedShutdownWorker(), database: noOpCloser{}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Run(ctx, Config{}); err != nil {
		t.Fatalf("Run() = %v", err)
	}
}

func TestRuntimeResourcesServeConfiguredOAuthRoutes(t *testing.T) {
	cfg := testRuntimeConfig(t)
	resources, err := newRuntimeResources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeRuntimeResources(resources) }()

	request := httptest.NewRequest(http.MethodGet, "/oauth/client-metadata.json", nil)
	recorder := httptest.NewRecorder()
	resources.httpServer.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("metadata status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestRuntimeResourcesServeHealthRoute(t *testing.T) {
	cfg := testRuntimeConfig(t)
	resources, err := newRuntimeResources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeRuntimeResources(resources) }()

	recorder := httptest.NewRecorder()
	resources.httpServer.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestRuntimeResourcesRejectsInvalidRuntimeSeed(t *testing.T) {
	cfg := testRuntimeConfig(t)
	cfg.Shared.MasterSeed = "not-base64"
	_, err := newRuntimeResources(cfg)
	if err == nil {
		t.Fatal("newRuntimeResources() succeeded with an invalid runtime seed")
	}
}

func TestRunServesConfiguredOAuthRoutes(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(probe.Addr().(*net.TCPAddr).Port)
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	signingKey := testOAuthSigningKey(t)
	databasePath := t.TempDir() + "/bridge.db"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := testRuntimeConfig(t)
	cfg.Shared.Port = port
	cfg.Shared.DatabasePath = databasePath
	cfg.Bluesky.OAuthClientSigningKey = signingKey
	go func() {
		done <- Run(ctx, cfg)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run() = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not stop")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get("http://127.0.0.1:" + port + "/oauth/client-metadata.json")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("metadata status = %d", response.StatusCode)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("OAuth metadata route was not reachable")
}

func testOAuthSigningKey(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

func testRuntimeConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Shared: SharedConfig{
			Host: "127.0.0.1", Port: "0", DatabasePath: t.TempDir() + "/bridge.db",
			MasterSeed: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", RelayURL: "wss://relay.example",
			RelayManagementURL: "https://relay.example/manage", RelayCanonicalURL: "https://relay.example/manage",
			RelayAdminPrivateKey: strings.Repeat("1", 64), OutboxLimit: 100, OutboxPollInterval: time.Millisecond,
		},
		Bluesky: BlueskyConfig{
			AccountDID: "did:plc:owner", BaseURL: "https://bsky.example",
			OAuthCallbackURL: "https://bridge.example/oauth/callback", OAuthAuthorizationServerURL: "https://issuer.example",
			OAuthClientID: "https://bridge.example/oauth/client-metadata.json", OAuthClientSigningKey: testOAuthSigningKey(t),
			OAuthEncryptionKey: base64.StdEncoding.EncodeToString(make([]byte, 32)),
		},
	}
}

func TestRunPropagatesDispatcherFailureAndClosesResourcesOnce(t *testing.T) {
	old := newRuntimeResources
	t.Cleanup(func() { newRuntimeResources = old })
	worker := startWorker(func(context.Context) error { return errors.New("dispatcher failed") })
	jetstream, database := completedShutdownWorker(), &countingCloser{}
	newRuntimeResources = func(Config) (runtimeResources, error) {
		return runtimeResources{httpServer: &http.Server{Addr: "127.0.0.1:0"}, jetstream: jetstream, dispatcher: worker, dispatcherDone: worker.Done(), database: database}, nil
	}
	err := Run(context.Background(), Config{})
	if err == nil || !strings.Contains(err.Error(), "dispatcher failed") {
		t.Fatalf("Run() = %v", err)
	}
	if !jetstream.canceled || database.count != 1 {
		t.Fatalf("shutdown = jetstream canceled %v, database closes %d", jetstream.canceled, database.count)
	}
}

type countingCloser struct{ count int }

func (c *countingCloser) Close() error                       { c.count++; return nil }
func (c *countingCloser) CloseContext(context.Context) error { return c.Close() }

type blockingCloser struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	count   int
}

func newBlockingCloser() *blockingCloser {
	return &blockingCloser{started: make(chan struct{}), release: make(chan struct{})}
}

func (c *blockingCloser) Close() error {
	c.mu.Lock()
	c.count++
	if c.count == 1 {
		close(c.started)
	}
	c.mu.Unlock()
	<-c.release
	return nil
}

func (c *blockingCloser) Count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.count }

func TestTrackedDatabaseCloseContextTimesOutThenJoinsSameClose(t *testing.T) {
	underlying := newBlockingCloser()
	database := newTrackedDatabaseCloser(underlying)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := database.CloseContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first CloseContext() = %v", err)
	}
	<-underlying.started
	if got := underlying.Count(); got != 1 {
		t.Fatalf("Close() starts = %d", got)
	}
	close(underlying.release)
	if err := database.CloseContext(context.Background()); err != nil {
		t.Fatalf("second CloseContext() = %v", err)
	}
	if got := underlying.Count(); got != 1 {
		t.Fatalf("Close() starts after join = %d", got)
	}
}

func TestTrackedDatabaseCloseContextNormalCloseStartsOnce(t *testing.T) {
	underlying := &countingCloser{}
	database := newTrackedDatabaseCloser(underlying)
	if err := database.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if underlying.count != 1 {
		t.Fatalf("Close() starts = %d", underlying.count)
	}
}

type shutdownTestWorker struct {
	canceled bool
	done     chan struct{}
	order    *[]string
	name     string
}

func completedShutdownWorker() *shutdownTestWorker {
	done := make(chan struct{})
	close(done)
	return &shutdownTestWorker{done: done}
}

func (w *shutdownTestWorker) Cancel() { w.canceled = true }
func (w *shutdownTestWorker) Wait(ctx context.Context) error {
	select {
	case <-w.done:
		if w.order != nil {
			*w.order = append(*w.order, w.name)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type orderedCloser struct {
	order *[]string
	name  string
}

func (c orderedCloser) Close() error                       { *c.order = append(*c.order, c.name); return nil }
func (c orderedCloser) CloseContext(context.Context) error { return c.Close() }

func TestCloseRuntimeResourcesTimeoutDoesNotCloseDatabaseAndCancelsAllWorkers(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 25 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = old })
	blocked := &shutdownTestWorker{done: make(chan struct{})}
	finished := completedShutdownWorker()
	database := &countingCloser{}
	started := time.Now()
	err := closeRuntimeResources(runtimeResources{jetstream: blocked, dispatcher: finished, database: database})
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("closeRuntimeResources() = %v", err)
	}
	if time.Since(started) > 250*time.Millisecond {
		t.Fatalf("shutdown was not bounded: %s", time.Since(started))
	}
	if !blocked.canceled || !finished.canceled {
		t.Fatalf("worker cancellation = blocked %v, finished %v", blocked.canceled, finished.canceled)
	}
	if database.count != 0 {
		t.Fatalf("database close count = %d", database.count)
	}
}

func TestCloseRuntimeResourcesWaitsForWorkersBeforeClosingDatabase(t *testing.T) {
	order := []string{}
	jetstream := completedShutdownWorker()
	jetstream.order, jetstream.name = &order, "jetstream"
	dispatcher := completedShutdownWorker()
	dispatcher.order, dispatcher.name = &order, "dispatcher"
	err := closeRuntimeResources(runtimeResources{jetstream: jetstream, dispatcher: dispatcher, database: orderedCloser{&order, "database"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "jetstream,dispatcher,database" {
		t.Fatalf("shutdown order = %s", got)
	}
}

func TestCloseRuntimeResourcesBoundsDatabaseClose(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 20 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = old })
	underlying := newBlockingCloser()
	database := newTrackedDatabaseCloser(underlying)
	started := time.Now()
	err := closeRuntimeResources(runtimeResources{
		jetstream: completedShutdownWorker(),
		database:  database,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("closeRuntimeResources() = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("database shutdown was not bounded: %s", elapsed)
	}
	if got := underlying.Count(); got != 1 {
		t.Fatalf("Close() starts = %d", got)
	}
	close(underlying.release)
	if err := database.CloseContext(context.Background()); err != nil {
		t.Fatalf("join database close: %v", err)
	}
}

type signalingDatabaseCloser struct{ closed chan struct{} }

func (c signalingDatabaseCloser) CloseContext(context.Context) error {
	close(c.closed)
	return nil
}

func TestCloseRuntimeResourcesJoinsHTTPHandlersBeforeClosingDatabase(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHandler)
	handlerExited := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
		close(handlerExited)
	}))
	server.Start()
	t.Cleanup(server.Close)
	requestDone := make(chan struct{})
	go func() {
		response, err := server.Client().Get(server.URL)
		if err == nil {
			_ = response.Body.Close()
		}
		close(requestDone)
	}()
	<-entered

	databaseClosed := make(chan struct{})
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- closeRuntimeResources(runtimeResources{
			httpServer: server.Config,
			jetstream:  completedShutdownWorker(),
			database:   signalingDatabaseCloser{closed: databaseClosed},
		})
	}()
	select {
	case <-databaseClosed:
		t.Fatal("database closed while HTTP handler was still running")
	case <-time.After(20 * time.Millisecond):
	}
	releaseHandler()
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-handlerExited:
	default:
		t.Fatal("HTTP handler was not joined")
	}
	<-requestDone
}

func TestCloseRuntimeResourcesHTTPDeadlineKeepsDatabaseOpen(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 20 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = old })
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	server.Start()
	t.Cleanup(func() {
		releaseHandler()
		server.Close()
	})
	requestDone := make(chan struct{})
	go func() {
		response, err := server.Client().Get(server.URL)
		if err == nil {
			_ = response.Body.Close()
		}
		close(requestDone)
	}()
	<-entered
	database := &countingCloser{}
	err := closeRuntimeResources(runtimeResources{
		httpServer: server.Config,
		jetstream:  completedShutdownWorker(),
		database:   database,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("closeRuntimeResources() = %v", err)
	}
	if database.count != 0 {
		t.Fatalf("database close count = %d", database.count)
	}
	releaseHandler()
	<-requestDone
	server.Close()
}

func TestWorkerCloseContextStopsWaitingAtDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	w := startWorker(func(context.Context) error { <-release; return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := w.CloseContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseContext() = %v", err)
	}
}
