// Package observability provides structured logging and safe metadata helpers.
package observability

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// NewLogger returns a JSON logger with a fixed minimum level.
func NewLogger(output io.Writer, level slog.Level) *slog.Logger {
	handler := traceHandler{slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})}
	return slog.New(handler)
}

type traceHandler struct{ next slog.Handler }

func (handler traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.next.Enabled(ctx, level)
}

func (handler traceHandler) Handle(ctx context.Context, record slog.Record) error {
	span := trace.SpanContextFromContext(ctx)
	if span.IsValid() {
		record.AddAttrs(slog.String("trace_id", span.TraceID().String()), slog.String("span_id", span.SpanID().String()), slog.Bool("trace_sampled", span.IsSampled()))
	}
	return handler.next.Handle(ctx, record)
}

func (handler traceHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	return traceHandler{next: handler.next.WithAttrs(attributes)}
}

func (handler traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{next: handler.next.WithGroup(name)}
}
