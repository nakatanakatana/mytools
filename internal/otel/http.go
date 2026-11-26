package otel

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// ShouldTraceHTTPRequest excludes health, readiness, and metrics probe paths from tracing.
func ShouldTraceHTTPRequest(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	switch r.URL.Path {
	case "/healthz", "/readyz", "/metrics":
		return false
	default:
		return true
	}
}

// HTTPRouteFromPattern extracts the path template from Go 1.22+ http.Request.Pattern
// (e.g. "GET /api/status" -> "/api/status", "example.com/foo" -> "/foo").
func HTTPRouteFromPattern(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ""
	}
	if _, route, ok := strings.Cut(pattern, " "); ok {
		pattern = strings.TrimSpace(route)
	}
	if idx := strings.IndexByte(pattern, '/'); idx >= 0 {
		return pattern[idx:]
	}
	return ""
}

// NewHTTPHandler wraps an http.Handler with OpenTelemetry tracing while excluding probe endpoints
// and recording http.route and the span name from Go 1.22+ ServeMux r.Pattern when matched.
func NewHTTPHandler(handler http.Handler, operation string) http.Handler {
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if route := HTTPRouteFromPattern(r.Pattern); route != "" {
				span := trace.SpanFromContext(r.Context())
				span.SetAttributes(semconv.HTTPRoute(route))
				if r.Method != "" {
					span.SetName(r.Method + " " + route)
				} else {
					span.SetName(route)
				}
			}
		}()
		handler.ServeHTTP(w, r)
	})
	return otelhttp.NewHandler(
		wrapped,
		operation,
		otelhttp.WithFilter(ShouldTraceHTTPRequest),
	)
}

type dynamicDefaultTransport struct{}

func (dynamicDefaultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return http.DefaultTransport.RoundTrip(req)
}

// NewHTTPTransport wraps base with otelhttp. When base is nil, it delegates dynamically
// to http.DefaultTransport so tests replacing http.DefaultTransport continue to work.
func NewHTTPTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = dynamicDefaultTransport{}
	}
	return otelhttp.NewTransport(base)
}

// NewHTTPClient returns a copy of base (or a new http.Client if nil) whose Transport is wrapped with otelhttp.
func NewHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		return &http.Client{Transport: NewHTTPTransport(nil)}
	}
	cloned := *base
	cloned.Transport = NewHTTPTransport(cloned.Transport)
	return &cloned
}

// InjectHeaders injects W3C traceparent and baggage headers from ctx into header.
func InjectHeaders(ctx context.Context, header http.Header) {
	if header == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(header))
}
