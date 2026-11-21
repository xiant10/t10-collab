package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func recorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	if _, err := Setup(context.Background(), "t10-test", ""); err != nil {
		t.Fatal(err)
	}
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

func attr(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

func TestWrapMarksSessionAndNamesByPattern(t *testing.T) {
	rec := recorder(t)
	// Services register the wrapped handler on a mux, which sets the pattern.
	h := http.NewServeMux()
	h.Handle("GET /v1/items/{id}", Wrap("GET /v1/items/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	req := httptest.NewRequest("GET", "/v1/items/itm_1?q=secret+words", nil)
	req.Header.Set(SessionHeader, "ses_abc123")
	h.ServeHTTP(httptest.NewRecorder(), req)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	if got := spans[0].Name(); got != "GET /v1/items/{id}" {
		t.Fatalf("span name %q", got)
	}
	if got, _ := attr(spans[0], SessionAttribute); got != "ses_abc123" {
		t.Fatalf("session %q", got)
	}
}

func TestSessionDropsFreeText(t *testing.T) {
	rec := recorder(t)
	h := Wrap("GET /x", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(SessionHeader, "my session, with words")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if _, ok := attr(rec.Ended()[0], SessionAttribute); ok {
		t.Fatal("free text went on the span")
	}
}

func TestWrapAllNamesByMatchedRoute(t *testing.T) {
	rec := recorder(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/things/{id}", func(w http.ResponseWriter, r *http.Request) {})
	WrapAll(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/things/thg_9", nil))
	if got := rec.Ended()[0].Name(); got != "GET /v1/things/{id}" {
		t.Fatalf("span name %q", got)
	}
}

func TestContinueJoinsSentTrace(t *testing.T) {
	rec := recorder(t)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	_, end := Continue(context.Background(), "00-"+traceID+"-00f067aa0ba902b7-01", "tool.call")
	end()
	if got := rec.Ended()[0].SpanContext().TraceID().String(); got != traceID {
		t.Fatalf("trace %s", got)
	}
}

func TestInjectCarriesTrace(t *testing.T) {
	recorder(t)
	ctx, span := Tracer().Start(context.Background(), "outgoing")
	defer span.End()
	h := http.Header{}
	Inject(ctx, h)
	if h.Get("traceparent") == "" {
		t.Fatal("no traceparent")
	}
}

func TestSetupWithBadEndpointStaysOff(t *testing.T) {
	shutdown, err := Setup(context.Background(), "t10-test", "collector:4318")
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if Service() != "t10-test" {
		t.Fatalf("service %q", Service())
	}
}
