// Package observability provides structured logging and safe metadata helpers.
package observability

import (
	"io"
	"log/slog"
)

// NewLogger returns a JSON logger with a fixed minimum level.
func NewLogger(output io.Writer, level slog.Level) *slog.Logger {
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}
