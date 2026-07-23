package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/mastodon"
	bridgeoauth "github.com/nakatanakatana/mytools/cmd/nostr-bridge/oauth"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/outbox"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/relayclient"
	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load configuration: %v\n", err)
		os.Exit(1)
	}
	if err := Run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "nostr-bridge failed: %v\n", err)
		os.Exit(1)
	}
}

// ServerAddress returns the TCP address on which the HTTP server will listen.
func ServerAddress(cfg Config) string {
	return cfg.Shared.Host + ":" + cfg.Shared.Port
}

// RegisterOAuthRoutes attaches the OAuth client endpoints to the bridge HTTP server.
// The OAuth client serves the start/callback endpoints as well as the public
// client metadata and JWKS routes under /oauth/.
func RegisterOAuthRoutes(mux *http.ServeMux, blueskyClient *bridgeoauth.Client, mastodonClient *mastodon.OAuthClient) {
	if blueskyClient != nil {
		mux.Handle("/oauth/bluesky/", blueskyClient.HandlerAt("/oauth/bluesky"))
	}
	if mastodonClient != nil {
		mux.HandleFunc("POST /oauth/mastodon/start", func(w http.ResponseWriter, r *http.Request) {
			u, err := mastodonClient.StartAuthorization(r.Context())
			if err != nil {
				http.Error(w, "could not start OAuth authorization", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"authorization_url":%q}`, u)
		})
		mux.HandleFunc("GET /oauth/mastodon/callback", mastodonClient.HandleCallback)
	}
}

type runtimeResources struct {
	httpServer           *http.Server
	jetstream            shutdownWorker
	dispatcher           shutdownWorker
	dispatcherDone       <-chan struct{}
	oauthMaintenance     shutdownWorker
	oauthMaintenanceDone <-chan struct{}
	database             databaseCloser
}

type databaseCloser interface {
	CloseContext(context.Context) error
}

// newRuntimeResources is a seam for constructing the process-lifetime integrations.
var newRuntimeResources = func(cfg Config) (runtimeResources, error) {
	if seed, err := base64.StdEncoding.DecodeString(cfg.Shared.MasterSeed); err != nil {
		return runtimeResources{}, fmt.Errorf("decode bridge master seed: %w", err)
	} else if len(seed) != 32 {
		return runtimeResources{}, errInvalidMasterSeed
	}
	bridgeStore, database, err := bridgestore.Open(context.Background(), cfg.Shared.DatabasePath)
	if err != nil {
		return runtimeResources{}, err
	}
	client, mastodonOAuth, err := newOAuthClients(cfg, bridgeStore)
	if err != nil {
		_ = database.Close()
		return runtimeResources{}, err
	}
	health := NewHealth(HealthOptions{
		DatabaseCheck: func(ctx context.Context) error {
			pinger, ok := database.(interface{ PingContext(context.Context) error })
			if !ok {
				return errors.New("database does not support health checks")
			}
			return pinger.PingContext(ctx)
		},
		OutboxCount: bridgeStore.OutboxCount, OutboxLimit: int64(cfg.Shared.OutboxLimit), RequireDispatcher: true, EnabledProviders: enabledProviders(cfg),
	})
	managementURL, err := url.Parse(cfg.Shared.RelayManagementURL)
	if err != nil {
		_ = database.Close()
		return runtimeResources{}, err
	}
	canonicalURL, err := url.Parse(cfg.Shared.RelayCanonicalURL)
	if err != nil {
		_ = database.Close()
		return runtimeResources{}, err
	}
	adminKey, err := nostr.SecretKeyFromHex(cfg.Shared.RelayAdminPrivateKey)
	if err != nil {
		_ = database.Close()
		return runtimeResources{}, err
	}
	managementClient, err := relayclient.NewHTTPManagementClient(managementURL, canonicalURL, adminKey)
	if err != nil {
		_ = database.Close()
		return runtimeResources{}, err
	}
	dispatcher := &outbox.Dispatcher{Store: bridgeStore, Management: managementClient, Publisher: &relayclient.WebSocketPublisher{RelayURL: cfg.Shared.RelayURL}, PollInterval: cfg.Shared.OutboxPollInterval, Observer: healthRelayObserver{health}}
	health.Update(func(m *HealthMetrics) { m.DispatcherRunning = true })
	dispatchWorker := startWorker(func(ctx context.Context) error {
		defer health.Update(func(m *HealthMetrics) { m.DispatcherRunning = false })
		return dispatcher.Run(ctx)
	})
	var oauthMaintenance *workerCloser
	if client != nil {
		oauthMaintenance = startBlueskyOAuthMaintenance(cfg.Bluesky, client, health)
	}
	trackedDatabase := newTrackedDatabaseCloser(database)
	runtime, err := newRuntimeSync(cfg, bridgeStore, client, mastodonOAuth, health)
	if err != nil {
		resources := runtimeResources{
			dispatcher: dispatchWorker,
			database:   trackedDatabase,
		}
		if oauthMaintenance != nil {
			resources.oauthMaintenance = oauthMaintenance
			resources.oauthMaintenanceDone = oauthMaintenance.Done()
		}
		return runtimeResources{}, closeRuntimeConstructionFailure(err, resources)
	}
	mux := http.NewServeMux()
	RegisterOAuthRoutes(mux, client, mastodonOAuth)
	RegisterHealthRoutes(mux, health)
	resources := runtimeResources{
		httpServer:     &http.Server{Addr: ServerAddress(cfg), Handler: mux},
		jetstream:      runtime,
		dispatcher:     dispatchWorker,
		dispatcherDone: dispatchWorker.Done(),
		database:       trackedDatabase,
	}
	if oauthMaintenance != nil {
		resources.oauthMaintenance = oauthMaintenance
		resources.oauthMaintenanceDone = oauthMaintenance.Done()
	}
	return resources, nil
}

func closeRuntimeConstructionFailure(constructionErr error, resources runtimeResources) error {
	return errors.Join(constructionErr, closeRuntimeResources(resources))
}

func enabledProviders(cfg Config) []string {
	var p []string
	if cfg.Bluesky.Enabled() {
		p = append(p, "bluesky")
	}
	if cfg.Mastodon.Enabled() {
		p = append(p, "mastodon")
	}
	return p
}

func newOAuthClients(cfg Config, store bridgestore.OAuthStore) (*bridgeoauth.Client, *mastodon.OAuthClient, error) {
	var b *bridgeoauth.Client
	var m *mastodon.OAuthClient
	var err error
	if cfg.Bluesky.Enabled() {
		b, err = newOAuthClient(cfg, store)
		if err != nil {
			return nil, nil, err
		}
	}
	if cfg.Mastodon.Enabled() {
		key, decodeErr := base64.StdEncoding.DecodeString(cfg.Mastodon.OAuthEncryptionKey)
		if decodeErr != nil {
			return nil, nil, fmt.Errorf("decode Mastodon OAuth encryption key: %w", decodeErr)
		}
		account := normalizedMastodonAccount(cfg.Mastodon.Account, cfg.Mastodon.BaseURL)
		m, err = mastodon.NewOAuthClient(mastodon.OAuthOptions{Scope: bridgestore.SourceScope{Provider: "mastodon", Account: account}, Store: store, BaseURL: cfg.Mastodon.BaseURL, Account: account, ClientID: cfg.Mastodon.OAuthClientID, ClientSecret: cfg.Mastodon.OAuthClientSecret, RedirectURL: cfg.Mastodon.OAuthCallbackURL, EncryptionKey: key})
		if err != nil {
			return nil, nil, fmt.Errorf("construct Mastodon OAuth client: %w", err)
		}
	}
	return b, m, nil
}

func normalizedMastodonAccount(account, baseURL string) string {
	u, _ := url.Parse(baseURL)
	account = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(account, "@")))
	if !strings.Contains(account, "@") {
		account += "@" + strings.ToLower(u.Hostname())
	}
	return account
}

func newOAuthClient(cfg Config, bridgeStore bridgestore.OAuthStore) (*bridgeoauth.Client, error) {
	signingKeyDER, err := base64.StdEncoding.DecodeString(cfg.Bluesky.OAuthClientSigningKey)
	if err != nil {
		return nil, fmt.Errorf("decode OAuth client signing key: %w", err)
	}
	signingKeyValue, err := x509.ParsePKCS8PrivateKey(signingKeyDER)
	if err != nil {
		return nil, fmt.Errorf("parse OAuth client signing key: %w", err)
	}
	signingKey, ok := signingKeyValue.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("OAuth client signing key must be an ECDSA private key")
	}
	if signingKey.Curve != elliptic.P256() {
		return nil, errors.New("OAuth client signing key must use P-256 for ES256")
	}
	encryptionKey, err := base64.StdEncoding.DecodeString(cfg.Bluesky.OAuthEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("decode OAuth encryption key: %w", err)
	}
	client, err := bridgeoauth.NewClient(bridgeoauth.Options{
		Scope:                  bridgestore.SourceScope{Provider: "bluesky", Account: cfg.Bluesky.AccountDID},
		Store:                  bridgeStore,
		AuthorizationServerURL: cfg.Bluesky.OAuthAuthorizationServerURL,
		ClientID:               cfg.Bluesky.OAuthClientID,
		RedirectURL:            cfg.Bluesky.OAuthCallbackURL,
		ClientSigningKey:       signingKey,
		EncryptionKey:          encryptionKey,
	})
	if err != nil {
		return nil, fmt.Errorf("construct OAuth client: %w", err)
	}
	return client, nil
}

type noOpCloser struct{}

func (noOpCloser) Close() error                       { return nil }
func (noOpCloser) CloseContext(context.Context) error { return nil }

// trackedDatabaseCloser owns the single database Close operation even when a
// caller's shutdown deadline expires. Later callers join that same operation;
// no second Close is started and no untracked generic closer goroutine exists.
type trackedDatabaseCloser struct {
	closer io.Closer
	once   sync.Once
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

func newTrackedDatabaseCloser(closer io.Closer) *trackedDatabaseCloser {
	return &trackedDatabaseCloser{closer: closer, done: make(chan struct{})}
}

func (c *trackedDatabaseCloser) CloseContext(ctx context.Context) error {
	c.once.Do(func() {
		go func() {
			err := c.closer.Close()
			c.mu.Lock()
			c.err = err
			c.mu.Unlock()
			close(c.done)
		}()
	})
	select {
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type workerCloser struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

var shutdownTimeout = 10 * time.Second

type shutdownWorker interface {
	Cancel()
	Wait(context.Context) error
}

func startWorker(run func(context.Context) error) *workerCloser {
	ctx, cancel := context.WithCancel(context.Background())
	w := &workerCloser{cancel: cancel, done: make(chan struct{})}
	go func() {
		err := run(ctx)
		w.mu.Lock()
		w.err = err
		w.mu.Unlock()
		close(w.done)
	}()
	return w
}
func (w *workerCloser) Done() <-chan struct{} { return w.done }
func (w *workerCloser) Err() error            { w.mu.Lock(); defer w.mu.Unlock(); return w.err }

func (w *workerCloser) Cancel() { w.cancel() }

func (w *workerCloser) Wait(ctx context.Context) error {
	select {
	case <-w.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	err := w.Err()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (w *workerCloser) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return w.CloseContext(ctx)
}
func (w *workerCloser) CloseContext(ctx context.Context) error {
	w.Cancel()
	return w.Wait(ctx)
}

type healthRelayObserver struct{ health *Health }

func (o healthRelayObserver) RelayDelivered(at time.Time) {
	o.health.Update(func(m *HealthMetrics) { m.LastRelayDelivery = at })
}

// Run owns the process-lifetime resources. Service behavior is added in later
// tasks; for now it only constructs and reliably shuts down those resources.
func Run(ctx context.Context, cfg Config) error {
	resources, err := newRuntimeResources(cfg)
	if err != nil {
		return fmt.Errorf("construct runtime resources: %w", err)
	}

	if ctx.Err() == nil && resources.httpServer != nil {
		serveErr := make(chan error, 1)
		go func() { serveErr <- resources.httpServer.ListenAndServe() }()
		select {
		case err := <-serveErr:
			if !errors.Is(err, http.ErrServerClosed) {
				return errors.Join(fmt.Errorf("serve HTTP: %w", err), closeRuntimeResources(resources))
			}
		case <-ctx.Done():
		case <-resources.dispatcherDone:
			return runtimeWorkerStopped(ctx, resources, "dispatcher", resources.dispatcher)
		case <-resources.oauthMaintenanceDone:
			return runtimeWorkerStopped(ctx, resources, "OAuth maintenance", resources.oauthMaintenance)
		}
	}

	return closeRuntimeResources(resources)
}

func runtimeWorkerStopped(ctx context.Context, resources runtimeResources, name string, worker shutdownWorker) error {
	var workerErr error
	if result, ok := worker.(interface{ Err() error }); ok {
		workerErr = result.Err()
	}
	if ctx.Err() != nil || errors.Is(workerErr, context.Canceled) {
		return closeRuntimeResources(resources)
	}
	if workerErr == nil {
		workerErr = errors.New("worker stopped unexpectedly")
	}
	return errors.Join(fmt.Errorf("%s: %w", name, workerErr), closeRuntimeResources(resources))
}

func closeRuntimeResources(resources runtimeResources) error {
	var shutdownErrs []error
	safeToCloseDatabase := true
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// Initiate every worker cancellation before waiting for HTTP handlers or
	// any one worker. The single context bounds the entire HTTP/worker phase.
	for _, worker := range []shutdownWorker{
		resources.jetstream,
		resources.dispatcher,
		resources.oauthMaintenance,
	} {
		if worker != nil {
			worker.Cancel()
		}
	}
	if resources.httpServer != nil {
		if err := resources.httpServer.Shutdown(ctx); err != nil {
			shutdownErrs = append(shutdownErrs, fmt.Errorf("stop HTTP server: %w", err))
			safeToCloseDatabase = false
		}
	}

	for _, item := range []struct {
		name   string
		worker shutdownWorker
	}{
		{"stop Jetstream", resources.jetstream},
		{"stop dispatcher", resources.dispatcher},
		{"stop OAuth maintenance", resources.oauthMaintenance},
	} {
		if item.worker != nil {
			if err := item.worker.Wait(ctx); err != nil {
				shutdownErrs = append(shutdownErrs, fmt.Errorf("%s: %w", item.name, err))
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					safeToCloseDatabase = false
				}
			}
		}
	}
	if safeToCloseDatabase && resources.database != nil {
		databaseCtx, databaseCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer databaseCancel()
		err := resources.database.CloseContext(databaseCtx)
		if err != nil {
			shutdownErrs = append(shutdownErrs, fmt.Errorf("close database: %w", err))
		}
	}
	return errors.Join(shutdownErrs...)
}
