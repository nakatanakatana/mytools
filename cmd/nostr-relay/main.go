package main

import (
	"context"
	"fmt"
	"log/slog"
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
	return fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)
}

func Run() error {
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	logger := setupLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	relay, closeRelay, err := NewRelay(cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize relay: %w", err)
	}
	defer closeRelay()

	server := &http.Server{
		Addr:    ServerAddress(cfg),
		Handler: relay,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErrors:
		if err == http.ErrServerClosed {
			return nil
		}
		return fmt.Errorf("server error: %w", err)
	case sig := <-shutdown:
		logger.Info("shutdown started", "signal", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			if closeErr := server.Close(); closeErr != nil {
				return fmt.Errorf("could not close server after shutdown failure %w: %w", err, closeErr)
			}
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
