package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestWrapContinuesAnIncomingTrace(t *testing.T) {
	if _, err := Setup(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	var got string
	h := Wrap("GET /x", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = trace.SpanContextFromContext(r.Context()).TraceID().String()
	}))
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("trace ID = %q", got)
	}
}

func TestContinueJoinsTheTraceInAMessage(t *testing.T) {
	if _, err := Setup(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	ctx, end := Continue(context.Background(), "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", "tool work_get")
	defer end()
	if got := trace.SpanContextFromContext(ctx).TraceID().String(); got != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("trace ID = %q", got)
	}
}

func TestContinueWithoutATraceparentKeepsTheContext(t *testing.T) {
	if _, err := Setup(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	ctx, end := Continue(context.Background(), "not a traceparent", "tool work_get")
	defer end()
	if trace.SpanContextFromContext(ctx).TraceID().IsValid() {
		t.Error("a bad traceparent started a trace")
	}
}
