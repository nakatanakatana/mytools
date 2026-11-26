package otel

import (
	"context"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
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

// NewHTTPHandler wraps an http.Handler with OpenTelemetry tracing while excluding probe endpoints.
func NewHTTPHandler(handler http.Handler, operation string) http.Handler {
	return otelhttp.NewHandler(
		handler,
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
