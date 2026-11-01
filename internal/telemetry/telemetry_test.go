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
