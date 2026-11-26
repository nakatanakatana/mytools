package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	_ "modernc.org/sqlite"
)

func saveAndRestoreOTelGlobals(t *testing.T) {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
}

func TestSetupNoopWhenEndpointUnset(t *testing.T) {
	saveAndRestoreOTelGlobals(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_TRACES_EXPORTER", "")

	shutdown, err := Setup(t.Context(), "test-service")
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	_, span := otel.Tracer("test").Start(t.Context(), "op")
	if span.SpanContext().IsValid() {
		t.Fatalf("expected no-op span when endpoint is unset, got valid span context %v", span.SpanContext())
	}
	span.End()
	if err := shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func TestSetupNoopWhenTracesExporterNone(t *testing.T) {
	saveAndRestoreOTelGlobals(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")

	shutdown, err := Setup(t.Context(), "test-service")
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	_, span := otel.Tracer("test").Start(t.Context(), "op")
	if span.SpanContext().IsValid() {
		t.Fatalf("expected no-op span when OTEL_TRACES_EXPORTER=none")
	}
	span.End()
	if err := shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func TestSetupOTLPHTTPExportsSpansOnShutdown(t *testing.T) {
	saveAndRestoreOTelGlobals(t)
	var received atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_SERVICE_NAME", "custom-svc")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=test")
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")

	shutdown, err := Setup(t.Context(), "default-svc")
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	_, span := otel.Tracer("test").Start(t.Context(), "work")
	if !span.SpanContext().IsValid() {
		t.Fatal("expected valid span context when OTLP endpoint is configured")
	}
	span.End()

	if err := ShutdownWithTimeout(shutdown, 2*time.Second); err != nil {
		t.Fatalf("ShutdownWithTimeout() error = %v", err)
	}
	if received.Load() == 0 {
		t.Fatal("expected OTLP HTTP collector to receive exported spans on shutdown")
	}
}

func TestSetupOTLPGRPCInitializes(t *testing.T) {
	saveAndRestoreOTelGlobals(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")

	shutdown, err := Setup(t.Context(), "grpc-svc")
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_ = shutdown(ctx)
}

func TestSetupRejectsUnsupportedValues(t *testing.T) {
	saveAndRestoreOTelGlobals(t)

	t.Run("unsupported exporter", func(t *testing.T) {
		t.Setenv("OTEL_TRACES_EXPORTER", "jaeger")
		if _, err := Setup(t.Context(), "svc"); err == nil {
			t.Fatal("expected error for unsupported OTEL_TRACES_EXPORTER")
		}
	})

	t.Run("unsupported protocol", func(t *testing.T) {
		t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "thrift")
		if _, err := Setup(t.Context(), "svc"); err == nil {
			t.Fatal("expected error for unsupported OTEL_EXPORTER_OTLP_PROTOCOL")
		}
	})
}

func TestBuildResourceRespectsEnvOverrides(t *testing.T) {
	t.Run("default service name", func(t *testing.T) {
		t.Setenv("OTEL_SERVICE_NAME", "")
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
		res, err := buildResource(t.Context(), "nostr-relay")
		if err != nil {
			t.Fatalf("buildResource() error = %v", err)
		}
		got, ok := res.Set().Value(semconv.ServiceNameKey)
		if !ok || got.AsString() != "nostr-relay" {
			t.Fatalf("service.name = %q (ok=%v), want nostr-relay", got.AsString(), ok)
		}
	})

	t.Run("env service name and attributes", func(t *testing.T) {
		t.Setenv("OTEL_SERVICE_NAME", "override-relay")
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.namespace=nostr,env=staging")
		res, err := buildResource(t.Context(), "nostr-relay")
		if err != nil {
			t.Fatalf("buildResource() error = %v", err)
		}
		got, ok := res.Set().Value(semconv.ServiceNameKey)
		if !ok || got.AsString() != "override-relay" {
			t.Fatalf("service.name = %q (ok=%v), want override-relay", got.AsString(), ok)
		}
		envAttr, ok := res.Set().Value(attribute.Key("env"))
		if !ok || envAttr.AsString() != "staging" {
			t.Fatalf("env = %q (ok=%v), want staging", envAttr.AsString(), ok)
		}
	})
}

func TestHTTPInstrumentationPropagatesW3CAndExcludesProbes(t *testing.T) {
	saveAndRestoreOTelGlobals(t)
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(newPropagator())

	var gotBaggage string
	var gotTraceID trace.TraceID
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/data", func(w http.ResponseWriter, r *http.Request) {
		gotBaggage = baggage.FromContext(r.Context()).Member("tenant").Value()
		gotTraceID = trace.SpanContextFromContext(r.Context()).TraceID()
		w.WriteHeader(http.StatusOK)
	})
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	}

	server := httptest.NewServer(NewHTTPHandler(mux, "test-server"))
	defer server.Close()

	client := NewHTTPClient(server.Client())

	member, err := baggage.NewMember("tenant", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatal(err)
	}
	rootCtx := baggage.ContextWithBaggage(t.Context(), bag)
	rootCtx, rootSpan := otel.Tracer("test").Start(rootCtx, "root")

	req, err := http.NewRequestWithContext(rootCtx, http.MethodGet, server.URL+"/api/data", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	rootSpan.End()

	if gotBaggage != "alpha" {
		t.Fatalf("propagated baggage tenant = %q, want alpha", gotBaggage)
	}
	if gotTraceID != rootSpan.SpanContext().TraceID() {
		t.Fatalf("server trace_id = %s, want %s", gotTraceID, rootSpan.SpanContext().TraceID())
	}

	beforeProbeCount := len(recorder.Ended())
	plainClient := server.Client()
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		r, err := plainClient.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s error = %v", path, err)
		}
		_ = r.Body.Close()
	}
	if after := len(recorder.Ended()); after != beforeProbeCount {
		t.Fatalf("probe requests created %d spans, want 0", after-beforeProbeCount)
	}
}

func TestOpenSQLiteRecordsPlaceholderSQLWithoutBindValues(t *testing.T) {
	saveAndRestoreOTelGlobals(t)
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v", err)
	}
	defer func() { _ = db.Close() }()

	parentCtx, parentSpan := otel.Tracer("test").Start(t.Context(), "db-parent")
	secretValue := "super-secret-bind-value-12345"

	tx, err := db.BeginTx(parentCtx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(parentCtx, `CREATE TABLE items (id INTEGER PRIMARY KEY, secret TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(parentCtx, `INSERT INTO items (id, secret) VALUES (?, ?)`, 1, secretValue); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var got string
	if err := db.QueryRowContext(parentCtx, `SELECT secret FROM items WHERE id = ?`, 1).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != secretValue {
		t.Fatalf("queried secret = %q, want %q", got, secretValue)
	}
	parentSpan.End()

	spans := recorder.Ended()
	var foundSelect, foundInsert bool
	for _, sp := range spans {
		if sp.Name() == "db-parent" {
			continue
		}
		if sp.SpanContext().TraceID() != parentSpan.SpanContext().TraceID() {
			t.Errorf("SQL span %q did not inherit parent trace ID", sp.Name())
		}
		hasSQLiteSystem := false
		for _, attr := range sp.Attributes() {
			if (attr.Key == "db.system.name" || attr.Key == "db.system") && attr.Value.AsString() == "sqlite" {
				hasSQLiteSystem = true
			}
			val := attr.Value.String()
			if strings.Contains(val, secretValue) {
				t.Fatalf("SQL span %q leaked raw bind value in attribute %s=%q", sp.Name(), attr.Key, val)
			}
			if attr.Key == "db.query.text" || attr.Key == "db.statement" {
				if strings.Contains(val, "INSERT INTO items (id, secret) VALUES (?, ?)") {
					foundInsert = true
				}
				if strings.Contains(val, "SELECT secret FROM items WHERE id = ?") {
					foundSelect = true
				}
			}
		}
		if !hasSQLiteSystem {
			t.Errorf("SQL span %q missing db.system.name=sqlite attribute", sp.Name())
		}
	}
	if !foundInsert || !foundSelect {
		t.Fatalf("expected placeholder INSERT (found=%v) and SELECT (found=%v) spans", foundInsert, foundSelect)
	}
}

func TestLoggerIncludesTraceAndSpanIDs(t *testing.T) {
	saveAndRestoreOTelGlobals(t)
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)

	var buf bytes.Buffer
	logger := NewLogger(&buf, slog.LevelInfo)

	ctx, span := otel.Tracer("test").Start(t.Context(), "logged-op")
	logger.InfoContext(ctx, "with span", "k", "v")
	span.End()

	logger.InfoContext(t.Context(), "without span")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 log lines, got %d: %s", len(lines), buf.String())
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decode first log line: %v", err)
	}
	if first["trace_id"] != span.SpanContext().TraceID().String() {
		t.Fatalf("trace_id = %v, want %s", first["trace_id"], span.SpanContext().TraceID().String())
	}
	if first["span_id"] != span.SpanContext().SpanID().String() {
		t.Fatalf("span_id = %v, want %s", first["span_id"], span.SpanContext().SpanID().String())
	}

	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("decode second log line: %v", err)
	}
	if _, ok := second["trace_id"]; ok {
		t.Fatalf("unexpected trace_id in log without span: %v", second)
	}
	if _, ok := second["span_id"]; ok {
		t.Fatalf("unexpected span_id in log without span: %v", second)
	}
}

func TestShutdownWithTimeoutPassesDeadline(t *testing.T) {
	var hasDeadline bool
	wantErr := errors.New("flush failed")
	err := ShutdownWithTimeout(func(ctx context.Context) error {
		_, hasDeadline = ctx.Deadline()
		return wantErr
	}, 250*time.Millisecond)
	if !errors.Is(err, wantErr) {
		t.Fatalf("ShutdownWithTimeout() error = %v, want %v", err, wantErr)
	}
	if !hasDeadline {
		t.Fatal("expected shutdown context to have a deadline")
	}
}
