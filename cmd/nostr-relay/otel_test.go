package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/relayclient"
	appotel "github.com/nakatanakatana/mytools/internal/otel"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func installTestTracerProvider(t *testing.T) *tracetest.SpanRecorder {
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

func TestRunShutsDownOTelAfterClosingResources(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server := newFakeRelayServer()
	var eventsMu sync.Mutex
	var events []string
	record := func(event string) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, event)
	}
	server.record = record

	var otelShutdownHadDeadline bool
	deps := runtimeDependencies{
		newRelay: func(context.Context, Config) (RelayResources, error) {
			return RelayResources{
				Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
				Close: func() error {
					record("resources-close")
					return nil
				},
			}, nil
		},
		newServer: func(string, http.Handler) relayServer { return server },
		setupOTel: func(context.Context, string) (func(context.Context) error, error) {
			return func(shutdownCtx context.Context) error {
				_, otelShutdownHadDeadline = shutdownCtx.Deadline()
				record("otel-shutdown")
				return nil
			}, nil
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, Config{Host: "127.0.0.1", Port: "3334"}, deps)
	}()
	<-server.started
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runWithDependencies() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runWithDependencies() did not stop after context cancellation")
	}

	if !otelShutdownHadDeadline {
		t.Fatal("expected OTel shutdown to receive a bounded timeout context")
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	want := []string{"serve", "shutdown", "serve-drained", "resources-close", "otel-shutdown"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle events = %v, want %v", events, want)
	}
}

func TestBridgeToRelayManagementTracePropagationAndSQLiteSpans(t *testing.T) {
	recorder := installTestTracerProvider(t)

	adminSK := nostr.Generate()
	adminPK := adminSK.Public()
	readerPK := nostr.Generate().Public()
	publisherPK := nostr.Generate().Public()
	secretReason := "secret-reason-value-do-not-leak"

	var server *httptest.Server
	var receivedBaggage string
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("placeholder handler should be replaced")
	}))
	defer server.Close()

	cfg := Config{
		Mode:          ModePrivateSQLite,
		DatabasePath:  filepath.Join(t.TempDir(), "relay.db"),
		ServiceURL:    server.URL + "/manage",
		AdminPubkey:   adminPK.Hex(),
		ReaderPubkeys: []string{readerPK.Hex()},
	}
	resources, err := NewRelay(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewRelay() error = %v", err)
	}
	defer func() { _ = resources.Close() }()

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wrap inner check around the instrumented ManagementHandler? No: extract from headers or wrap inner.
		// Verify baggage propagates through ManagementHandler's request context by inspecting header + traceparent.
		receivedBaggage = r.Header.Get("Baggage")
		resources.ManagementHandler.ServeHTTP(w, r)
	})

	endpoint, err := url.Parse(server.URL + "/manage")
	if err != nil {
		t.Fatal(err)
	}
	client, err := relayclient.NewHTTPManagementClient(endpoint, endpoint, adminSK)
	if err != nil {
		t.Fatal(err)
	}
	client.HTTPClient = appotel.NewHTTPClient(server.Client())

	member, err := baggage.NewMember("bridge.op", "allow")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatal(err)
	}
	rootCtx := baggage.ContextWithBaggage(t.Context(), bag)
	rootCtx, rootSpan := otel.Tracer("test-bridge").Start(rootCtx, "bridge.allow_publisher")

	if err := client.AllowPubKey(rootCtx, publisherPK, secretReason); err != nil {
		t.Fatalf("AllowPubKey() error = %v", err)
	}
	rootSpan.End()

	if !strings.Contains(receivedBaggage, "bridge.op=allow") {
		t.Fatalf("Baggage header = %q, want bridge.op=allow", receivedBaggage)
	}

	rootTraceID := rootSpan.SpanContext().TraceID()
	var sawServerSpan, sawSQLiteSpan bool
	for _, sp := range recorder.Ended() {
		if sp.SpanContext().TraceID() != rootTraceID {
			continue
		}
		if sp.SpanKind() == trace.SpanKindServer {
			sawServerSpan = true
		}
		for _, attr := range sp.Attributes() {
			if strings.Contains(attr.Value.Emit(), secretReason) {
				t.Fatalf("span %q leaked bind parameter value in %s", sp.Name(), attr.Key)
			}
			if (attr.Key == "db.system.name" || attr.Key == "db.system") && attr.Value.AsString() == "sqlite" {
				sawSQLiteSpan = true
			}
		}
	}
	if !sawServerSpan {
		t.Fatal("expected nostr-relay-management server span sharing bridge root trace ID")
	}
	if !sawSQLiteSpan {
		t.Fatal("expected SQLite child span sharing bridge root trace ID")
	}
}

func TestBridgeToRelayWebSocketPublishTracePropagation(t *testing.T) {
	recorder := installTestTracerProvider(t)

	adminSK := nostr.Generate()
	readerPK := nostr.Generate().Public()
	publisherSK := nostr.Generate()

	cfg := Config{
		Mode:          ModePrivateSQLite,
		DatabasePath:  filepath.Join(t.TempDir(), "relay.db"),
		ServiceURL:    "http://relay.example",
		AdminPubkey:   adminSK.Public().Hex(),
		ReaderPubkeys: []string{readerPK.Hex()},
	}
	resources, err := NewRelay(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewRelay() error = %v", err)
	}
	defer func() { _ = resources.Close() }()

	// Pre-allow publisher via management handler using direct HTTP call.
	mgmtServer := httptest.NewServer(resources.ManagementHandler)
	defer mgmtServer.Close()
	mgmtURL, _ := url.Parse(mgmtServer.URL)
	canonicalURL, _ := url.Parse(cfg.ServiceURL)
	mgmtClient, err := relayclient.NewHTTPManagementClient(mgmtURL, canonicalURL, adminSK)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgmtClient.AllowPubKey(t.Context(), publisherSK.Public(), "test"); err != nil {
		t.Fatalf("AllowPubKey() error = %v", err)
	}

	var gotTraceparent string
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("Traceparent")
		resources.ProtocolHandler.ServeHTTP(w, r)
	}))
	defer wsServer.Close()

	event := nostr.Event{
		Kind:      nostr.KindTextNote,
		CreatedAt: nostr.Now(),
		Content:   "hello traced websocket",
	}
	if err := event.Sign(publisherSK); err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
	pub := &relayclient.WebSocketPublisher{RelayURL: wsURL}

	rootCtx, rootSpan := otel.Tracer("test-bridge").Start(t.Context(), "bridge.publish")
	if err := pub.Publish(rootCtx, event); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	rootSpan.End()

	wantTraceID := rootSpan.SpanContext().TraceID().String()
	if !strings.Contains(gotTraceparent, wantTraceID) {
		t.Fatalf("WebSocket upgrade Traceparent = %q, want trace ID %s", gotTraceparent, wantTraceID)
	}

	var sawRelayServerSpan, sawSQLiteSpan bool
	for _, sp := range recorder.Ended() {
		if sp.SpanContext().TraceID() != rootSpan.SpanContext().TraceID() {
			continue
		}
		if sp.SpanKind() == trace.SpanKindServer {
			sawRelayServerSpan = true
		}
		for _, attr := range sp.Attributes() {
			if (attr.Key == "db.system.name" || attr.Key == "db.system") && attr.Value.AsString() == "sqlite" {
				sawSQLiteSpan = true
			}
		}
	}
	if !sawRelayServerSpan {
		t.Fatal("expected relay server span linked to WebSocket publish trace ID")
	}
	if !sawSQLiteSpan {
		t.Fatal("expected SQLite child span linked to WebSocket publish trace ID")
	}
}
