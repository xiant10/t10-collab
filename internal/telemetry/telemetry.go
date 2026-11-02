// Package telemetry exports request traces over OTLP. Spans carry routes and
// statuses only, never a query string or a body, because those hold people's
// words. With no endpoint it does nothing.
package telemetry

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const serviceName = "t10-collab"

// Setup starts the exporter and returns a shutdown that flushes it.
func Setup(ctx context.Context, endpoint string) (func(context.Context) error, error) {
	// Without this, otelhttp drops an incoming traceparent.
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", serviceName)))
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}
	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, fmt.Errorf("dial otlp exporter at %q: %w", endpoint, err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

// Wrap traces one route, named by its pattern so that IDs stay out of the name.
func Wrap(pattern string, next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, pattern)
}

// Continue starts a span under a traceparent that came inside a message, as MCP
// sends it, instead of in a header. The name must hold no record text.
func Continue(ctx context.Context, traceparent, name string) (context.Context, func()) {
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
	ctx, span := otel.Tracer(serviceName).Start(ctx, name)
	return ctx, func() { span.End() }
}
