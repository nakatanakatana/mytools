package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func installBridgeTestTracerProvider(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return recorder
}

func TestRunShutsDownOTelWithTimeout(t *testing.T) {
	oldNewRuntimeResources := newRuntimeResources
	oldSetupOTel := setupOTel
	t.Cleanup(func() {
		newRuntimeResources = oldNewRuntimeResources
		setupOTel = oldSetupOTel
	})

	var shutdownCalled bool
	var shutdownHasDeadline bool
	setupOTel = func(context.Context, string) (func(context.Context) error, error) {
		return func(ctx context.Context) error {
			shutdownCalled = true
			_, shutdownHasDeadline = ctx.Deadline()
			return nil
		}, nil
	}
	newRuntimeResources = func(Config) (runtimeResources, error) {
		return runtimeResources{
			httpServer: &http.Server{},
			jetstream:  completedShutdownWorker(),
			database:   noOpCloser{},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Run(ctx, Config{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !shutdownCalled {
		t.Fatal("expected OTel shutdown to be called on exit")
	}
	if !shutdownHasDeadline {
		t.Fatal("expected OTel shutdown context to have a deadline")
	}
}

func TestRuntimeHTTPServerExcludesProbesAndPropagatesTrace(t *testing.T) {
	recorder := installBridgeTestTracerProvider(t)

	resources, err := newRuntimeResources(testRuntimeConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeRuntimeResources(resources) }()

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		rec := httptest.NewRecorder()
		resources.httpServer.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	}
	for _, sp := range recorder.Ended() {
		if sp.SpanKind() == trace.SpanKindServer {
			t.Fatalf("unexpected server span %q recorded for probe request", sp.Name())
		}
	}

	parentCtx, parentSpan := otel.Tracer("test-caller").Start(t.Context(), "caller")
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	otel.GetTextMapPropagator().Inject(parentCtx, propagation.HeaderCarrier(req.Header))
	rec := httptest.NewRecorder()
	resources.httpServer.Handler.ServeHTTP(rec, req)
	parentSpan.End()

	if rec.Code != http.StatusOK {
		t.Fatalf("/api/status code = %d, want 200", rec.Code)
	}
	var foundServerSpan bool
	for _, sp := range recorder.Ended() {
		if sp.SpanKind() == trace.SpanKindServer && sp.SpanContext().TraceID() == parentSpan.SpanContext().TraceID() {
			foundServerSpan = true
		}
	}
	if !foundServerSpan {
		t.Fatal("expected server span for /api/status linked to incoming traceparent")
	}
}

func TestBridgeStoreRecordsSQLitePlaceholderSpans(t *testing.T) {
	recorder := installBridgeTestTracerProvider(t)

	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	st, closer, err := bridgestore.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = closer.Close() }()

	parentCtx, parentSpan := otel.Tracer("test-bridge").Start(t.Context(), "bridge.store_op")
	secretPubkey := strings.Repeat("a", 64)
	if err := st.SetPublisherRegistered(parentCtx, secretPubkey, time.Unix(1700000000, 0)); err != nil {
		t.Fatalf("SetPublisherRegistered() error = %v", err)
	}
	ok, err := st.PublisherRegistered(parentCtx, secretPubkey)
	if err != nil || !ok {
		t.Fatalf("PublisherRegistered() = %v, %v", ok, err)
	}
	parentSpan.End()

	var sawSQLite bool
	for _, sp := range recorder.Ended() {
		if sp.Name() == "bridge.store_op" || sp.SpanContext().TraceID() != parentSpan.SpanContext().TraceID() {
			continue
		}
		for _, attr := range sp.Attributes() {
			if strings.Contains(attr.Value.String(), secretPubkey) {
				t.Fatalf("SQL span %q leaked bind value in attribute %s", sp.Name(), attr.Key)
			}
			if (attr.Key == "db.system.name" || attr.Key == "db.system") && attr.Value.AsString() == "sqlite" {
				sawSQLite = true
			}
		}
	}
	if !sawSQLite {
		t.Fatal("expected SQLite child spans with db.system.name=sqlite")
	}
}
