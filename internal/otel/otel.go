package otel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace/noop"
)

// DefaultShutdownTimeout bounds flushing and shutting down the TracerProvider on exit.
const DefaultShutdownTimeout = 5 * time.Second

// Setup initializes OpenTelemetry tracing from standard OTEL_* environment variables.
// When no OTLP endpoint is configured or OTEL_TRACES_EXPORTER=none, it installs a no-op
// TracerProvider and returns a no-op shutdown function.
func Setup(ctx context.Context, defaultServiceName string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(newPropagator())

	exporterKind := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER")))
	switch exporterKind {
	case "none":
		otel.SetTracerProvider(noop.NewTracerProvider())
		return noopShutdown, nil
	case "", "otlp":
	default:
		return nil, fmt.Errorf("unsupported OTEL_TRACES_EXPORTER %q", exporterKind)
	}

	if !hasOTLPEndpoint() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return noopShutdown, nil
	}

	exporter, err := newOTLPExporter(ctx)
	if err != nil {
		return nil, err
	}

	res, err := buildResource(ctx, defaultServiceName)
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, fmt.Errorf("build OpenTelemetry resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return func(shutdownCtx context.Context) error {
		return errors.Join(tp.ForceFlush(shutdownCtx), tp.Shutdown(shutdownCtx))
	}, nil
}

// ShutdownWithTimeout invokes shutdown with a bounded context timeout.
func ShutdownWithTimeout(shutdown func(context.Context) error, timeout time.Duration) error {
	if shutdown == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return shutdown(ctx)
}

func noopShutdown(context.Context) error { return nil }

func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

func hasOTLPEndpoint() bool {
	return strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")) != "" ||
		strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != ""
}

func newOTLPExporter(ctx context.Context) (*otlptrace.Exporter, error) {
	protocol := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")))
	if protocol == "" {
		protocol = strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")))
	}
	switch protocol {
	case "", "http/protobuf":
		return otlptracehttp.New(ctx)
	case "grpc":
		return otlptracegrpc.New(ctx)
	default:
		return nil, fmt.Errorf("unsupported OTLP protocol %q", protocol)
	}
}

func buildResource(ctx context.Context, defaultServiceName string) (*resource.Resource, error) {
	base := resource.NewWithAttributes("", semconv.ServiceName(defaultServiceName))
	envRes, err := resource.New(
		ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, err
	}
	return resource.Merge(base, envRes)
}
