package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func ServerAddress(cfg Config) string {
	return net.JoinHostPort(addressHost(cfg.Host), cfg.Port)
}

func ManagementServerAddress(cfg Config) string {
	return net.JoinHostPort(addressHost(cfg.ManagementHost), cfg.ManagementPort)
}

func Run() error {
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, cfg)
}

func run(ctx context.Context, cfg Config) (runErr error) {
	return runWithDependencies(ctx, cfg, runtimeDependencies{
		newRelay: NewRelay,
		newServer: func(addr string, handler http.Handler) relayServer {
			return &http.Server{Addr: addr, Handler: handler}
		},
	})
}

type relayServer interface {
	ListenAndServe() error
	Shutdown(context.Context) error
	Close() error
}

type runtimeDependencies struct {
	newRelay  func(context.Context, Config) (RelayResources, error)
	newServer func(string, http.Handler) relayServer
}

func runWithDependencies(ctx context.Context, cfg Config, deps runtimeDependencies) (runErr error) {
	logger := setupLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	resources, err := deps.newRelay(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize relay: %w", err)
	}
	defer func() {
		if err := resources.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("failed to close relay resources: %w", err))
		}
	}()
	if ctx.Err() != nil {
		return nil
	}

	protocolHandler := resources.ProtocolHandler
	if protocolHandler == nil {
		protocolHandler = resources.Handler
	}
	servers := []relayServer{deps.newServer(ServerAddress(cfg), protocolHandler)}
	addresses := []string{ServerAddress(cfg)}
	if resources.ManagementHandler != nil {
		servers = append(servers, deps.newServer(ManagementServerAddress(cfg), resources.ManagementHandler))
		addresses = append(addresses, ManagementServerAddress(cfg))
	}

	serverErrors := make(chan error, len(servers))
	for i, server := range servers {
		go func(server relayServer, address string) {
			logger.Info("listening", "addr", address)
			serverErrors <- server.ListenAndServe()
		}(server, addresses[i])
	}

	select {
	case err := <-serverErrors:
		if err == http.ErrServerClosed {
			err = nil
		}
		for _, server := range servers {
			_ = server.Close()
		}
		for i := 1; i < len(servers); i++ {
			<-serverErrors
		}
		if err == nil {
			return nil
		}
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown started")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var shutdownErr error
		for _, server := range servers {
			if err := server.Shutdown(ctx); err != nil {
				shutdownErr = errors.Join(shutdownErr, fmt.Errorf("graceful server shutdown failed: %w", err))
				if closeErr := server.Close(); closeErr != nil {
					shutdownErr = errors.Join(shutdownErr, fmt.Errorf("could not close server: %w", closeErr))
				}
			}
		}
		for range servers {
			if err := <-serverErrors; err != nil && err != http.ErrServerClosed {
				shutdownErr = errors.Join(shutdownErr, fmt.Errorf("server error during shutdown: %w", err))
			}
		}
		if shutdownErr != nil {
			return shutdownErr
		}
		logger.Info("shutdown complete")
		return nil
	}
}

func setupLogger(level string) *slog.Logger {
	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slogLevel}))
}
