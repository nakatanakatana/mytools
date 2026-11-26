package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestHTTPRouteFromPattern(t *testing.T) {
	tests := []struct {
		pattern string
		want    string
	}{
		{pattern: "GET /api/status", want: "/api/status"},
		{pattern: "/oauth/bluesky/callback", want: "/oauth/bluesky/callback"},
		{pattern: "POST example.com/api/push/subscriptions", want: "/api/push/subscriptions"},
		{pattern: "example.com/status", want: "/status"},
		{pattern: "", want: ""},
		{pattern: "   ", want: ""},
	}
	for _, tt := range tests {
		if got := HTTPRouteFromPattern(tt.pattern); got != tt.want {
			t.Errorf("HTTPRouteFromPattern(%q) = %q, want %q", tt.pattern, got, tt.want)
		}
	}
}

func TestNewHTTPHandlerRecordsRouteAndSpanName(t *testing.T) {
	saveAndRestoreOTelGlobals(t)
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/items/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := NewHTTPHandler(mux, "nostr-bridge")

	req := httptest.NewRequest(http.MethodGet, "/api/items/42", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Name() != "GET /api/items/{id}" {
		t.Errorf("span name = %q, want %q", spans[0].Name(), "GET /api/items/{id}")
	}

	var gotRoute string
	for _, attr := range spans[0].Attributes() {
		if attr.Key == "http.route" {
			gotRoute = attr.Value.String()
		}
	}
	if gotRoute != "/api/items/{id}" {
		t.Errorf("http.route = %q, want %q", gotRoute, "/api/items/{id}")
	}
}
