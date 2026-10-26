package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// Incoming traceparent parents the server span; outbound calls still carry no trace or baggage headers.
func TestTracedHandlerPropagation(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	defer otel.SetTracerProvider(prev)

	var upstream http.Header
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstream = r.Header.Clone() }))
	defer provider.Close()
	client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)} // as setupTracing wires it
	h := tracedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(r.Context(), "GET", provider.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}))

	const traceID, parentID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	r := httptest.NewRequest("GET", "/api/x", nil)
	r.Header.Set("traceparent", "00-"+traceID+"-"+parentID+"-01")
	r.Header.Set("baggage", "user=alice")
	h.ServeHTTP(httptest.NewRecorder(), r)

	for _, k := range []string{"traceparent", "tracestate", "baggage"} {
		if v := upstream.Get(k); v != "" {
			t.Errorf("outbound %s = %q, want none", k, v)
		}
	}
	var server sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			server = s
		}
	}
	if server == nil {
		t.Fatalf("no server span in %d spans", len(sr.Ended()))
	}
	if got := server.SpanContext().TraceID().String(); got != traceID {
		t.Errorf("trace ID = %s, want %s", got, traceID)
	}
	if got := server.Parent().SpanID().String(); got != parentID || !server.Parent().IsRemote() {
		t.Errorf("parent = %s (remote %v), want remote %s", got, server.Parent().IsRemote(), parentID)
	}

	sr.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/health", nil))
	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			t.Error("/api/health was traced")
		}
	}
}
