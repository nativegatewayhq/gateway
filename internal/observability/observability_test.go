package observability

import (
	"bytes"
	"log/slog"
	"net/url"
	"strings"
	"testing"
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
