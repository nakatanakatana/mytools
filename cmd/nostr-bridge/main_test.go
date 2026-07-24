package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	request := httptest.NewRequest(http.MethodGet, "/oauth/bluesky/client-metadata.json", nil)
	recorder := httptest.NewRecorder()
	resources.httpServer.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("metadata status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var metadata map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if got := metadata["jwks_uri"]; got != "https://bridge.example/oauth/bluesky/jwks" {
		t.Fatalf("jwks_uri = %v", got)
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

func TestRuntimeOAuthCallbackPublishesAuthorizationHealthBeforeRedirect(t *testing.T) {
	var state string
	var issuer *httptest.Server
	issuer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                                issuer.URL,
				"authorization_endpoint":                issuer.URL + "/oauth/authorize",
				"token_endpoint":                        issuer.URL + "/oauth/token",
				"pushed_authorization_request_endpoint": issuer.URL + "/oauth/par",
				"require_pushed_authorization_requests": true,
			})
		case "/oauth/par":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			state = r.Form.Get("state")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"request_uri": "urn:request:runtime-callback",
				"expires_in":  600,
			})
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if got := r.Form.Get("grant_type"); got != "authorization_code" {
				t.Fatalf("grant_type = %q, want authorization_code", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "runtime-access-secret",
				"refresh_token": "runtime-refresh-secret",
				"sub":           "did:plc:owner",
				"scope":         "atproto",
				"expires_in":    3600,
			})
		default:
			t.Fatalf("unexpected issuer path %q", r.URL.Path)
		}
	}))
	defer issuer.Close()

	previousTransport := http.DefaultTransport
	http.DefaultTransport = issuer.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	cfg := testRuntimeConfig(t)
	cfg.Bluesky.OAuthAuthorizationServerURL = issuer.URL
	resources, err := newRuntimeResources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeRuntimeResources(resources) }()

	start := httptest.NewRecorder()
	resources.httpServer.Handler.ServeHTTP(
		start,
		httptest.NewRequest(
			http.MethodPost,
			"/oauth/bluesky/start",
			strings.NewReader(`{"handle":"alice.test"}`),
		),
	)
	if start.Code != http.StatusOK || state == "" {
		t.Fatalf("OAuth start = status %d state %q body %s", start.Code, state, start.Body.String())
	}

	callback := httptest.NewRecorder()
	resources.httpServer.Handler.ServeHTTP(
		callback,
		httptest.NewRequest(
			http.MethodGet,
			"/oauth/bluesky/callback?state="+url.QueryEscape(state)+
				"&code="+url.QueryEscape("runtime-authorization-code-secret")+
				"&iss="+url.QueryEscape(issuer.URL),
			nil,
		),
	)
	if callback.Code != http.StatusSeeOther {
		t.Fatalf("OAuth callback = status %d body %s", callback.Code, callback.Body.String())
	}

	ready := httptest.NewRecorder()
	resources.httpServer.Handler.ServeHTTP(
		ready,
		httptest.NewRequest(http.MethodGet, "/readyz", nil),
	)
	var readiness struct {
		Providers map[string]struct {
			AuthorizationAvailable bool `json:"authorization_available"`
			ReauthRequired         bool `json:"reauth_required"`
			Degraded               bool `json:"degraded"`
			AccessTokenExpired     bool `json:"access_token_expired"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(ready.Body).Decode(&readiness); err != nil {
		t.Fatal(err)
	}
	blueskyHealth := readiness.Providers["bluesky"]
	if !blueskyHealth.AuthorizationAvailable ||
		blueskyHealth.ReauthRequired ||
		blueskyHealth.Degraded ||
		blueskyHealth.AccessTokenExpired {
		t.Fatalf("Bluesky health immediately after callback = %#v", blueskyHealth)
	}

	metrics := httptest.NewRecorder()
	resources.httpServer.Handler.ServeHTTP(
		metrics,
		httptest.NewRequest(http.MethodGet, "/metrics", nil),
	)
	for _, sample := range []string{
		`nostr_bridge_provider_authorization_available{provider="bluesky"} 1`,
		`nostr_bridge_provider_oauth_refresh_success_total{provider="bluesky",reason="authorization_code"} 1`,
		`nostr_bridge_provider_oauth_refresh_executions_total{provider="bluesky",reason="authorization_code"} 1`,
		`nostr_bridge_provider_oauth_reauth_required{provider="bluesky"} 0`,
		`nostr_bridge_provider_oauth_degraded{provider="bluesky"} 0`,
		`nostr_bridge_provider_oauth_access_token_expired{provider="bluesky"} 0`,
	} {
		if !strings.Contains(metrics.Body.String(), sample) {
			t.Errorf("metrics immediately after callback missing %q:\n%s", sample, metrics.Body.String())
		}
	}
	for _, zeroTimestamp := range []string{
		`nostr_bridge_provider_oauth_last_success_timestamp_seconds{provider="bluesky"} 0.000`,
		`nostr_bridge_provider_oauth_next_refresh_timestamp_seconds{provider="bluesky"} 0.000`,
	} {
		if strings.Contains(metrics.Body.String(), zeroTimestamp) {
			t.Errorf("metrics retained zero callback timestamp %q:\n%s", zeroTimestamp, metrics.Body.String())
		}
	}
	for _, secret := range []string{
		"runtime-access-secret",
		"runtime-refresh-secret",
		"runtime-authorization-code-secret",
		state,
	} {
		if strings.Contains(ready.Body.String(), secret) || strings.Contains(metrics.Body.String(), secret) {
			t.Fatalf("health output exposed secret %q", secret)
		}
	}
}

func TestBlueskyStartupCreatesOAuthMaintenanceWorker(t *testing.T) {
	cfg := testRuntimeConfig(t)
	resources, err := newRuntimeResources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeRuntimeResources(resources) }()

	if resources.oauthMaintenance == nil || resources.oauthMaintenanceDone == nil {
		t.Fatal("Bluesky runtime has no OAuth maintenance worker")
	}
}

func TestMastodonOnlyStartupDoesNotCreateOAuthMaintenanceWorker(t *testing.T) {
	cfg := testRuntimeConfig(t)
	cfg.Bluesky = BlueskyConfig{}
	cfg.Mastodon = MastodonConfig{
		BaseURL:            "https://social.example",
		Account:            "owner",
		OAuthCallbackURL:   "https://bridge.example/oauth/mastodon/callback",
		OAuthClientID:      "mastodon-client",
		OAuthClientSecret:  "mastodon-secret",
		OAuthEncryptionKey: base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}
	resources, err := newRuntimeResources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeRuntimeResources(resources) }()

	if resources.oauthMaintenance != nil || resources.oauthMaintenanceDone != nil {
		t.Fatal("Mastodon-only runtime unexpectedly has a Bluesky OAuth maintenance worker")
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
		response, err := http.Get("http://127.0.0.1:" + port + "/oauth/bluesky/client-metadata.json")
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

func TestFreshBlueskyOAuthMaintenanceKeepsProcessAvailableAfterFirstCheck(t *testing.T) {
	var issuerRequests atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		issuerRequests.Add(1)
	}))
	defer issuer.Close()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(probe.Addr().(*net.TCPAddr).Port)
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := testRuntimeConfig(t)
	cfg.Shared.Port = port
	cfg.Bluesky.OAuthAuthorizationServerURL = issuer.URL
	cfg.Bluesky.OAuthRefreshCheckInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg)
	}()
	finished := false
	t.Cleanup(func() {
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("Run did not stop")
			}
		}
	})

	baseURL := "http://127.0.0.1:" + port
	deadline := time.Now().Add(5 * time.Second)
	metadataReady := false
	for time.Now().Before(deadline) {
		response, requestErr := http.Get(baseURL + "/oauth/bluesky/client-metadata.json")
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("metadata status = %d", response.StatusCode)
			}
			metadataReady = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !metadataReady {
		t.Fatal("OAuth metadata route was not reachable")
	}

	time.Sleep(10 * cfg.Bluesky.OAuthRefreshCheckInterval)
	select {
	case runErr := <-done:
		finished = true
		t.Fatalf("Run stopped after fresh authorization check: %v", runErr)
	default:
	}

	response, err := http.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	var readiness struct {
		Providers map[string]struct {
			AuthorizationAvailable   bool `json:"authorization_available"`
			MaintenanceWorkerRunning bool `json:"maintenance_worker_running"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(response.Body).Decode(&readiness); err != nil {
		t.Fatal(err)
	}
	bluesky := readiness.Providers["bluesky"]
	if bluesky.AuthorizationAvailable || !bluesky.MaintenanceWorkerRunning {
		t.Fatalf("fresh Bluesky readiness = %#v", bluesky)
	}
	if got := issuerRequests.Load(); got != 0 {
		t.Fatalf("fresh authorization maintenance made %d issuer HTTP requests", got)
	}

	response, err = http.Get(baseURL + "/oauth/bluesky/client-metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("metadata status after maintenance check = %d", response.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("Run() after cancellation = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
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
			OAuthCallbackURL: "https://bridge.example/oauth/bluesky/callback", OAuthAuthorizationServerURL: "https://issuer.example",
			OAuthClientID: "https://bridge.example/oauth/bluesky/client-metadata.json", OAuthClientSigningKey: testOAuthSigningKey(t),
			OAuthEncryptionKey:        base64.StdEncoding.EncodeToString(make([]byte, 32)),
			OAuthRefreshPeriod:        30 * 24 * time.Hour,
			OAuthRefreshCheckInterval: 24 * time.Hour,
		},
	}
}

func TestOAuthRoutesOnlyExistForEnabledProviders(t *testing.T) {
	cfg := testRuntimeConfig(t)
	resources, err := newRuntimeResources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeRuntimeResources(resources) }()
	for path, want := range map[string]int{"/oauth/bluesky/client-metadata.json": http.StatusOK, "/oauth/mastodon/callback": http.StatusNotFound, "/oauth/client-metadata.json": http.StatusNotFound} {
		r := httptest.NewRecorder()
		resources.httpServer.Handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		if r.Code != want {
			t.Fatalf("%s status = %d, want %d", path, r.Code, want)
		}
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

func TestRunPropagatesOAuthMaintenanceFailureAndClosesResourcesOnce(t *testing.T) {
	old := newRuntimeResources
	t.Cleanup(func() { newRuntimeResources = old })
	maintenance := startWorker(func(context.Context) error {
		return errors.New("bounded maintenance failure")
	})
	jetstream, dispatcher, database := completedShutdownWorker(), completedShutdownWorker(), &countingCloser{}
	newRuntimeResources = func(Config) (runtimeResources, error) {
		return runtimeResources{
			httpServer:           &http.Server{Addr: "127.0.0.1:0"},
			jetstream:            jetstream,
			dispatcher:           dispatcher,
			oauthMaintenance:     maintenance,
			oauthMaintenanceDone: maintenance.Done(),
			database:             database,
		}, nil
	}

	err := Run(context.Background(), Config{})
	if err == nil || !strings.Contains(err.Error(), "OAuth maintenance") || !strings.Contains(err.Error(), "bounded maintenance failure") {
		t.Fatalf("Run() = %v", err)
	}
	if !jetstream.canceled || !dispatcher.canceled || database.count != 1 {
		t.Fatalf(
			"shutdown = jetstream canceled %v, dispatcher canceled %v, database closes %d",
			jetstream.canceled,
			dispatcher.canceled,
			database.count,
		)
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

type constructionShutdownWorker struct {
	name        string
	canceled    bool
	done        chan struct{}
	cancelOrder *[]string
	waitOrder   *[]string
	allCanceled func() bool
}

func (w *constructionShutdownWorker) Cancel() {
	w.canceled = true
	*w.cancelOrder = append(*w.cancelOrder, w.name)
}

func (w *constructionShutdownWorker) Wait(ctx context.Context) error {
	if !w.allCanceled() {
		return errors.New("wait started before every worker was canceled")
	}
	*w.waitOrder = append(*w.waitOrder, w.name)
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestCloseRuntimeConstructionFailureCancelsAllWorkersBeforeWaitAndKeepsDatabaseOpenOnTimeout(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 20 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = old })

	cancelOrder, waitOrder := []string{}, []string{}
	dispatcherDone := make(chan struct{})
	close(dispatcherDone)
	dispatcher := &constructionShutdownWorker{
		name: "dispatcher", done: dispatcherDone,
		cancelOrder: &cancelOrder, waitOrder: &waitOrder,
	}
	maintenance := &constructionShutdownWorker{
		name: "maintenance", done: make(chan struct{}),
		cancelOrder: &cancelOrder, waitOrder: &waitOrder,
	}
	allCanceled := func() bool { return dispatcher.canceled && maintenance.canceled }
	dispatcher.allCanceled = allCanceled
	maintenance.allCanceled = allCanceled
	database := &countingCloser{}
	constructionErr := errors.New("construct runtime sync")

	err := closeRuntimeConstructionFailure(constructionErr, runtimeResources{
		dispatcher:       dispatcher,
		oauthMaintenance: maintenance,
		database:         database,
	})
	if !errors.Is(err, constructionErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("closeRuntimeConstructionFailure() = %v", err)
	}
	if got := strings.Join(cancelOrder, ","); got != "dispatcher,maintenance" {
		t.Fatalf("cancel order = %s", got)
	}
	if got := strings.Join(waitOrder, ","); got != "dispatcher,maintenance" {
		t.Fatalf("wait order = %s", got)
	}
	if database.count != 0 {
		t.Fatalf("database close count = %d", database.count)
	}
}

func TestCloseRuntimeResourcesTimeoutDoesNotCloseDatabaseAndCancelsAllWorkers(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 25 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = old })
	blocked := &shutdownTestWorker{done: make(chan struct{})}
	finished := completedShutdownWorker()
	maintenance := completedShutdownWorker()
	database := &countingCloser{}
	started := time.Now()
	err := closeRuntimeResources(runtimeResources{
		jetstream:        blocked,
		dispatcher:       finished,
		oauthMaintenance: maintenance,
		database:         database,
	})
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("closeRuntimeResources() = %v", err)
	}
	if time.Since(started) > 250*time.Millisecond {
		t.Fatalf("shutdown was not bounded: %s", time.Since(started))
	}
	if !blocked.canceled || !finished.canceled || !maintenance.canceled {
		t.Fatalf(
			"worker cancellation = blocked %v, finished %v, maintenance %v",
			blocked.canceled,
			finished.canceled,
			maintenance.canceled,
		)
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
	maintenance := completedShutdownWorker()
	maintenance.order, maintenance.name = &order, "maintenance"
	err := closeRuntimeResources(runtimeResources{
		jetstream:        jetstream,
		dispatcher:       dispatcher,
		oauthMaintenance: maintenance,
		database:         orderedCloser{&order, "database"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "jetstream,dispatcher,maintenance,database" {
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
