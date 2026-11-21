// Package telemetry exports a service's request traces over OTLP/HTTP. Spans
// carry routes, statuses and identifiers only, never a query string or a body,
// because those hold people's words. With no endpoint it does nothing.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sync/atomic"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// SessionHeader names the chat session a request belongs to. The tool gateway
// and Workbench send it; its value goes on the span as t10.session.id.
const SessionHeader = "X-Session-ID"

// SessionAttribute is the span attribute that finds every span of one chat.
const SessionAttribute = "t10.session.id"

var service atomic.Value // string

func init() { service.Store("t10") }

// Service is the name Setup was given, for a service that starts its own spans.
func Service() string { return service.Load().(string) }

// Tracer is the service's tracer.
func Tracer() trace.Tracer { return otel.Tracer(Service()) }

// Setup names the service, starts the exporter and returns a shutdown that
// flushes it. Options reach the tracer provider, such as an ID generator.
func Setup(ctx context.Context, name, endpoint string, options ...sdktrace.TracerProviderOption) (func(context.Context) error, error) {
	service.Store(name)
	// Without this, otelhttp drops an incoming traceparent.
	otel.SetTextMapPropagator(propagation.TraceContext{})
	off := func(context.Context) error { return nil }
	if endpoint == "" {
		return off, nil
	}
	target, err := url.Parse(endpoint)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		// Tracing never stops a service, but this has to be visible.
		slog.Error("traces are off: OTEL_EXPORTER_OTLP_ENDPOINT must be a URL such as http://collector:4318")
		return off, nil
	}
	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", name)))
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(target.JoinPath("v1/traces").String()))
	if err != nil {
		return nil, fmt.Errorf("start otlp exporter at %q: %w", endpoint, err)
	}
	options = append([]sdktrace.TracerProviderOption{sdktrace.WithBatcher(exporter), sdktrace.WithResource(res)}, options...)
	provider := sdktrace.NewTracerProvider(options...)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

// Wrap traces one route, named by its pattern so that IDs stay out of the name,
// and marks the span with the request's chat session.
func Wrap(pattern string, next http.Handler, options ...otelhttp.Option) http.Handler {
	return otelhttp.NewHandler(WithSession(next), pattern, options...)
}

// WrapAll traces every request through one handler, such as a whole mux, and
// renames each span after the route that matched, once it has.
func WrapAll(next http.Handler) http.Handler {
	return otelhttp.NewHandler(WithSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.Pattern != "" {
			trace.SpanFromContext(r.Context()).SetName(r.Pattern)
		}
	})), "request")
}

// Inject puts the trace on ctx into an outgoing request's headers as a W3C
// traceparent, so the service it calls continues it. Outside a span it adds nothing.
func Inject(ctx context.Context, h http.Header) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(h))
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
	ctx, span := Tracer().Start(ctx, name)
	return ctx, func() { span.End() }
}

// spanValue is what may go on a span from a request: an identifier, never free text.
var spanValue = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// SpanValue reports whether s may go on a span: an identifier, never free text.
func SpanValue(s string) bool { return spanValue.MatchString(s) }

// WithSession marks the request's span with the chat session it names, so the
// work shows up when someone looks at that session.
func WithSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if session := r.Header.Get(SessionHeader); SpanValue(session) {
			trace.SpanFromContext(r.Context()).SetAttributes(attribute.String(SessionAttribute, session))
		}
		next.ServeHTTP(w, r)
	})
}
