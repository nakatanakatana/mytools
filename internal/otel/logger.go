package otel

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// TraceContextHandler enriches structured logs with trace_id and span_id when ctx has an active span.
type TraceContextHandler struct {
	next slog.Handler
}

// NewTraceContextHandler wraps next to append trace_id and span_id from the active span context.
func NewTraceContextHandler(next slog.Handler) slog.Handler {
	return &TraceContextHandler{next: next}
}

func (h *TraceContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *TraceContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if spanCtx := trace.SpanContextFromContext(ctx); spanCtx.IsValid() {
		r = r.Clone()
		r.AddAttrs(
			slog.String("trace_id", spanCtx.TraceID().String()),
			slog.String("span_id", spanCtx.SpanID().String()),
		)
	}
	return h.next.Handle(ctx, r)
}

func (h *TraceContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceContextHandler{next: h.next.WithAttrs(attrs)}
}

func (h *TraceContextHandler) WithGroup(name string) slog.Handler {
	return &TraceContextHandler{next: h.next.WithGroup(name)}
}

// NewLogger returns a JSON slog.Logger that emits trace_id and span_id for active spans.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(NewTraceContextHandler(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})))
}

// ParseLogLevel parses a log level string, defaulting to slog.LevelInfo.
func ParseLogLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
