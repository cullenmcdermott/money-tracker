package main

import (
	"context"
	"net/http"
	"os"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

var tracer = otel.Tracer("money-tracker")

// setupTracing exports traces over OTLP/HTTP when OTEL_EXPORTER_OTLP_ENDPOINT (or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT)
// is set; the SDK reads the other standard OTEL_* variables (headers, sampler, OTEL_SERVICE_NAME,
// OTEL_RESOURCE_ATTRIBUTES). Unset, every span is a no-op. No global propagator is installed, so trace context is never
// sent to the SEC, SimpleFIN, Jev or OIDC; only tracedHandler accepts it, from Traefik.
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

// tracedHandler makes each request (bar health checks) a span. It takes W3C traceparent from Traefik, which always
// overwrites the client's with its own span, so app spans nest under the ingress trace. Baggage is not read.
// ponytail: any in-cluster pod can also set traceparent (worst case: a forged parent or an unsampled flag);
// a NetworkPolicy allowing only Traefik closes that.
func tracedHandler(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "http", otelhttp.WithPropagators(propagation.TraceContext{}),
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/api/health" }))
}
