package observability

import (
	"bytes"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"context"
	"go.opentelemetry.io/otel/trace"
)

func TestNewLoggerHonorsLevel(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := NewLogger(&output, slog.LevelWarn)
	logger.Info("hidden")
	logger.Warn("visible", "request_id", "req_test")

	logOutput := output.String()
	if strings.Contains(logOutput, "hidden") {
		t.Fatalf("debug output contained filtered log: %s", logOutput)
	}
	if !strings.Contains(logOutput, "visible") || !strings.Contains(logOutput, "req_test") {
		t.Fatalf("expected structured fields in output: %s", logOutput)
	}
}

func TestLoggerAddsTraceCorrelationOnlyFromContext(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(&output, slog.LevelInfo)
	traceID, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	spanID, _ := trace.SpanIDFromHex("0102030405060708")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled}))
	logger.InfoContext(ctx, "correlated")
	logger.Info("plain")
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], traceID.String()) || !strings.Contains(lines[0], spanID.String()) || strings.Contains(lines[1], "trace_id") {
		t.Fatalf("logs=%s", output.String())
	}
}

func TestRedactURL(t *testing.T) {
	t.Parallel()

	original, err := url.Parse("https://user:pass@example.test/path?key=secret&safe=value&API_KEY=other")
	if err != nil {
		t.Fatal(err)
	}
	redacted := RedactURL(original)

	if strings.Contains(redacted.String(), "secret") || strings.Contains(redacted.String(), "other") || redacted.User != nil {
		t.Fatalf("RedactURL() leaked a secret: %s", redacted)
	}
	if redacted.Query().Get("safe") != "value" {
		t.Fatalf("RedactURL() removed safe query value: %s", redacted)
	}
	if original.Query().Get("key") != "secret" || original.User == nil {
		t.Fatal("RedactURL() mutated input")
	}
}

func TestRedactURLNil(t *testing.T) {
	t.Parallel()

	if RedactURL(nil) != nil {
		t.Fatal("RedactURL(nil) must return nil")
	}
}
