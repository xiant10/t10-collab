// Package telemetry exports request traces over OTLP, with t10-modkit's
// telemetry. What's this service's own is here: its name.
package telemetry

import (
	"context"
	"net/http"

	shared "github.com/xiant10/t10-modkit/telemetry"
)

const serviceName = "t10-collab"

// Setup starts the exporter and returns a shutdown that flushes it.
func Setup(ctx context.Context, endpoint string) (func(context.Context) error, error) {
	return shared.Setup(ctx, serviceName, endpoint)
}

// Wrap traces one route, named by its pattern, and marks its chat session.
func Wrap(pattern string, next http.Handler) http.Handler {
	return shared.Wrap(pattern, next)
}

// Continue starts a span under a traceparent that came inside a message, as MCP
// sends it, instead of in a header. The name must hold no record text.
func Continue(ctx context.Context, traceparent, name string) (context.Context, func()) {
	return shared.Continue(ctx, traceparent, name)
}
