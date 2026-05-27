package main

import (
	"context"
	"log/slog"
	"testing"
)

func TestSetupLogger(t *testing.T) {
	// Test with valid level "debug"
	logger := SetupLogger("debug")
	if logger == nil {
		t.Fatal("SetupLogger returned nil")
	}
	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Expected debug level to be enabled")
	}

	// Test with invalid level (should default to info)
	logger = SetupLogger("invalid_level_string")
	if logger == nil {
		t.Fatal("SetupLogger returned nil for invalid level")
	}
	if logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Expected debug level to be disabled for invalid input (should be Info)")
	}
	if !logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Expected info level to be enabled for invalid input")
	}

	// Test with explicit "info"
	logger = SetupLogger("info")
	if !logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Expected info level to be enabled")
	}

	// Test with explicit "error"
	logger = SetupLogger("error")
	if logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Expected info level to be disabled for error level")
	}
	if !logger.Enabled(context.Background(), slog.LevelError) {
		t.Error("Expected error level to be enabled")
	}

	// Test with explicit "warn"
	logger = SetupLogger("warn")
	if logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Expected info level to be disabled for warn level")
	}
	if !logger.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("Expected warn level to be enabled")
	}

	// Test with explicit "warning"
	logger = SetupLogger("warning")
	if !logger.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("Expected warn level to be enabled for 'warning' input")
	}
}
