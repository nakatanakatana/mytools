// Command sandbox-control runs the HTTP Web UI + Tailscale preview proxy (:8080)
// and the embedded SSH gateway (:2222) for managing Kubernetes agent-sandbox resources.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"charm.land/ssh"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/config"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/githubauth"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/k8s"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/service"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/sshgw"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/web"
	"golang.org/x/sync/errgroup"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(os.Args[1:], logger); err != nil {
		logger.Error("sandbox-control exited with error", "error", err)
		os.Exit(1)
	}
}

func run(args []string, logger *slog.Logger) error {
	cfg, err := config.Parse(args)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	kubeClient, err := k8s.NewFromKubeconfig(cfg.Kubeconfig, cfg.Namespace)
	if err != nil {
		return fmt.Errorf("init kubernetes client: %w", err)
	}

	svc := service.New(cfg, kubeClient)

	keyStore := sshgw.NewKeyStore(sshgw.KeyStoreOptions{
		GitHubUsers:          cfg.SSHGitHubUsers,
		GitLabUsers:          cfg.SSHGitLabUsers,
		GitLabBaseURL:        cfg.SSHGitLabBaseURL,
		AuthorizedKeysPath:   cfg.SSHAuthorizedKeysPath,
		AuthorizedKeysInline: cfg.SSHAuthorizedKeys,
		InsecureAllowAny:     cfg.InsecureAllowAnySSHKey,
		Logger:               logger,
	})

	initCtx, initCancel := context.WithTimeout(ctx, 30*time.Second)
	err = keyStore.Refresh(initCtx)
	initCancel()
	if err != nil {
		return fmt.Errorf("load initial SSH authorized keys: %w", err)
	}
	logger.Info("loaded SSH authorized keys", "count", keyStore.Count(), "insecure_allow_any", cfg.InsecureAllowAnySSHKey)

	go keyStore.StartRefreshLoop(ctx, cfg.SSHKeysRefreshInterval)

	ghAuth := githubauth.NewManager(githubauth.Options{
		ClientID:            cfg.GitHubAppClientID,
		EgressProxyAdminURL: cfg.EgressProxyAdminURL,
		Logger:              logger,
	})
	defer ghAuth.Close()

	webHandler, err := web.NewServer(svc, ghAuth)
	if err != nil {
		return fmt.Errorf("init web server: %w", err)
	}

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           webHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	sshServer, err := sshgw.NewServer(svc, keyStore, ghAuth, logger)
	if err != nil {
		return fmt.Errorf("init ssh gateway: %w", err)
	}

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		logger.Info("starting HTTP server", "addr", cfg.HTTPAddr, "namespace", cfg.Namespace, "router_url", cfg.RouterURL)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		logger.Info("starting SSH gateway", "addr", cfg.SSHAddr)
		if err := sshServer.ListenAndServe(); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
			return fmt.Errorf("ssh server: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		<-gCtx.Done()
		logger.Info("shutting down sandbox-control")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		var errs []error
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, fmt.Errorf("http shutdown: %w", err))
		}
		if err := sshServer.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, fmt.Errorf("ssh shutdown: %w", err))
		}
		return errors.Join(errs...)
	})

	return g.Wait()
}
