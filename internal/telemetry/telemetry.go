// Package telemetry exports request traces over OTLP. Spans carry routes and
// statuses only, never a query string or a body, because those hold people's
// words. With no endpoint it does nothing.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const serviceName = "t10-collab"

// Setup starts the exporter and returns a shutdown that flushes it.
func Setup(ctx context.Context, endpoint string) (func(context.Context) error, error) {
	// Without this, otelhttp drops an incoming traceparent.
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	target, err := url.Parse(endpoint)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		// Tracing never stops a service, but this has to be visible.
		slog.Error("traces are off: OTEL_EXPORTER_OTLP_ENDPOINT must be a URL such as http://collector:4318")
		return func(context.Context) error { return nil }, nil
	}
	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", serviceName)))
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(target.JoinPath("v1/traces").String()))
	if err != nil {
		return nil, fmt.Errorf("dial otlp exporter at %q: %w", endpoint, err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

// Wrap traces one route, named by its pattern so that IDs stay out of the name.
func Wrap(pattern string, next http.Handler) http.Handler {
	return otelhttp.NewHandler(withSession(next), pattern)
}

// Continue starts a span under a traceparent that came inside a message, as MCP
// sends it, instead of in a header. When the request's own span is already in
// that trace, as behind the tool gateway, it stays the parent. The name must hold
// no record text.
func Continue(ctx context.Context, traceparent, name string) (context.Context, func()) {
	sent := trace.SpanContextFromContext(otel.GetTextMapPropagator().Extract(context.Background(), propagation.MapCarrier{"traceparent": traceparent}))
	if current := trace.SpanContextFromContext(ctx); sent.IsValid() && sent.TraceID() != current.TraceID() {
		ctx = trace.ContextWithRemoteSpanContext(ctx, sent)
	}
	ctx, span := otel.Tracer(serviceName).Start(ctx, name)
	return ctx, func() { span.End() }
}

// spanValue is what may go on a span from a header: an identifier, never free text.
var spanValue = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// withSession marks the span with the chat session the tool gateway named, so
// this module's work shows up when someone looks at that session.
func withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if session := r.Header.Get("X-Session-ID"); spanValue.MatchString(session) {
			trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("t10.session.id", session))
		}
		next.ServeHTTP(w, r)
	})
}
