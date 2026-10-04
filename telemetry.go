package main

import (
	"context"
	"net/http"
	"os"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

var tracer = otel.Tracer("money-tracker")

// setupTracing exports traces over OTLP/HTTP when OTEL_EXPORTER_OTLP_ENDPOINT (or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT)
// is set; the SDK reads the other standard OTEL_* variables (headers, sampler, OTEL_SERVICE_NAME,
// OTEL_RESOURCE_ATTRIBUTES). Unset, every span is a no-op. No propagator is installed, so trace context is neither
// accepted from browsers nor sent to the SEC, SimpleFIN or Jev: every trace starts and ends in this service.
func setupTracing(ctx context.Context) (shutdown func(context.Context) error, err error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName("money-tracker")), resource.WithFromEnv(), resource.WithTelemetrySDK())
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	// Every outbound client here leaves Transport nil, so this traces SEC, SimpleFIN, Jev, OIDC and property calls.
	// otelhttp drops URL credentials and records no headers.
	http.DefaultTransport = otelhttp.NewTransport(http.DefaultTransport)
	return tp.Shutdown, nil
}
