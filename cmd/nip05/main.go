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

func Run() error {
	// 1. Load Configuration
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// 2. Setup Logger
	logger := SetupLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	logger.Info("Starting NIP-05 Server", "port", cfg.Port)

	// 3. Initialize Data Provider (File Generator)
	// We use the FileGenerator to pre-compute responses and store them in temp files.
	provider, err := NewFileGenerator(cfg.Mapping, cfg.Relays)
	if err != nil {
		return fmt.Errorf("failed to initialize file generator: %w", err)
	}
	defer provider.Cleanup()

	// 4. Initialize Handler
	handler := NewNIP05Handler(provider)
	router := AccessLogMiddleware(CORSMiddleware(handler))

	// 5. Setup Server Mux
	mux := http.NewServeMux()
	mux.Handle("/.well-known/nostr.json", router)

	// 6. Setup Server
	addr := fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// 7. Channel to listen for errors during server startup
	serverErrors := make(chan error, 1)

	// 8. Start Server in a goroutine
	go func() {
		logger.Info("Listening on", "addr", addr)
		serverErrors <- server.ListenAndServe()
	}()

	// 9. Channel to listen for shutdown signals
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	// 10. Blocking select for server errors or shutdown signals
	select {
	case err := <-serverErrors:
		return fmt.Errorf("server error: %w", err)

	case sig := <-shutdown:
		logger.Info("Shutdown started", "signal", sig)

		// Create context with timeout for graceful shutdown
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Attempt graceful shutdown
		if err := server.Shutdown(ctx); err != nil {
			logger.Error("Could not stop server gracefully", "error", err)
			if err := server.Close(); err != nil {
				return fmt.Errorf("could not close server: %w", err)
			}
		}
		logger.Info("Shutdown complete")
	}

	return nil
}
