package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(func(key string) (string, bool) {
		if key == "GATEWAY_DATABASE_URL" {
			return "postgres://gateway:test@localhost/gateway", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 10s", cfg.ShutdownTimeout)
	}
	if cfg.GoogleTimeout != 2*time.Minute {
		t.Errorf("GoogleTimeout = %v, want 2m", cfg.GoogleTimeout)
	}
	if cfg.GeminiBodyBytes != 32*1024*1024 {
		t.Errorf("GeminiBodyBytes = %d", cfg.GeminiBodyBytes)
	}
	if cfg.ImagesTimeout != 2*time.Minute || cfg.ImagesBodyBytes != 1024*1024 {
		t.Errorf("Images config = %v, %d", cfg.ImagesTimeout, cfg.ImagesBodyBytes)
	}
	if cfg.ImageEditsBodyBytes != 64*1024*1024 || cfg.ImageEditSpoolLimit != 8 {
		t.Errorf("Image edits config = %d, %d", cfg.ImageEditsBodyBytes, cfg.ImageEditSpoolLimit)
	}
	if cfg.BillingMode != BillingDisabled || cfg.MinimumMarginBPS != 0 {
		t.Errorf("Billing config = %q, %d", cfg.BillingMode, cfg.MinimumMarginBPS)
	}
	if cfg.ReplayBodyBytes != 32*1024*1024 {
		t.Errorf("ReplayBodyBytes = %d", cfg.ReplayBodyBytes)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"GATEWAY_HTTP_ADDR":                            "127.0.0.1:9090",
		"GATEWAY_LOG_LEVEL":                            "debug",
		"GATEWAY_SHUTDOWN_TIMEOUT":                     "3s",
		"GATEWAY_DATABASE_URL":                         "postgres://gateway:test@localhost/gateway",
		"GATEWAY_GOOGLE_REQUEST_TIMEOUT":               "90s",
		"GATEWAY_GEMINI_MAX_REQUEST_BODY_BYTES":        "1048576",
		"GATEWAY_OPENAI_IMAGES_REQUEST_TIMEOUT":        "80s",
		"GATEWAY_OPENAI_IMAGES_MAX_REQUEST_BODY_BYTES": "524288",
		"GATEWAY_IMAGE_EDITS_MAX_REQUEST_BODY_BYTES":   "33554432",
		"GATEWAY_IMAGE_EDIT_MAX_CONCURRENT_SPOOLS":     "4",
		"GATEWAY_BILLING_MODE":                         "required",
		"GATEWAY_MINIMUM_MARGIN_BPS":                   "1250",
		"GATEWAY_IDEMPOTENCY_MAX_RESPONSE_BYTES":       "16777216",
	}
	cfg, err := Load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9090" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 3*time.Second {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
	if cfg.GoogleTimeout != 90*time.Second || cfg.GeminiBodyBytes != 1048576 {
		t.Errorf("Gemini config = %v, %d", cfg.GoogleTimeout, cfg.GeminiBodyBytes)
	}
	if cfg.ImagesTimeout != 80*time.Second || cfg.ImagesBodyBytes != 524288 {
		t.Errorf("Images config = %v, %d", cfg.ImagesTimeout, cfg.ImagesBodyBytes)
	}
	if cfg.ImageEditsBodyBytes != 33554432 || cfg.ImageEditSpoolLimit != 4 {
		t.Errorf("Image edits config = %d, %d", cfg.ImageEditsBodyBytes, cfg.ImageEditSpoolLimit)
	}
	if cfg.BillingMode != BillingRequired || cfg.MinimumMarginBPS != 1250 {
		t.Errorf("Billing config = %q, %d", cfg.BillingMode, cfg.MinimumMarginBPS)
	}
	if cfg.ReplayBodyBytes != 16777216 {
		t.Errorf("ReplayBodyBytes = %d", cfg.ReplayBodyBytes)
	}
}

func TestLoadRejectsInvalidValuesWithoutEchoingThem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		key    string
		value  string
		marker string
	}{
		{name: "empty address", key: "GATEWAY_HTTP_ADDR", value: "", marker: ""},
		{name: "missing port", key: "GATEWAY_HTTP_ADDR", value: "secret-host", marker: "secret-host"},
		{name: "invalid port", key: "GATEWAY_HTTP_ADDR", value: "localhost:secret-port", marker: "secret-port"},
		{name: "invalid log level", key: "GATEWAY_LOG_LEVEL", value: "secret-level", marker: "secret-level"},
		{name: "invalid duration", key: "GATEWAY_SHUTDOWN_TIMEOUT", value: "secret-duration", marker: "secret-duration"},
		{name: "zero duration", key: "GATEWAY_SHUTDOWN_TIMEOUT", value: "0s", marker: ""},
		{name: "invalid google timeout", key: "GATEWAY_GOOGLE_REQUEST_TIMEOUT", value: "secret-timeout", marker: "secret-timeout"},
		{name: "excessive google timeout", key: "GATEWAY_GOOGLE_REQUEST_TIMEOUT", value: "11m", marker: ""},
		{name: "invalid gemini body limit", key: "GATEWAY_GEMINI_MAX_REQUEST_BODY_BYTES", value: "secret-size", marker: "secret-size"},
		{name: "excessive gemini body limit", key: "GATEWAY_GEMINI_MAX_REQUEST_BODY_BYTES", value: "33554433", marker: ""},
		{name: "invalid images timeout", key: "GATEWAY_OPENAI_IMAGES_REQUEST_TIMEOUT", value: "secret-timeout", marker: "secret-timeout"},
		{name: "excessive images timeout", key: "GATEWAY_OPENAI_IMAGES_REQUEST_TIMEOUT", value: "11m", marker: ""},
		{name: "invalid images body limit", key: "GATEWAY_OPENAI_IMAGES_MAX_REQUEST_BODY_BYTES", value: "secret-size", marker: "secret-size"},
		{name: "excessive images body limit", key: "GATEWAY_OPENAI_IMAGES_MAX_REQUEST_BODY_BYTES", value: "1048577", marker: ""},
		{name: "invalid edit body limit", key: "GATEWAY_IMAGE_EDITS_MAX_REQUEST_BODY_BYTES", value: "secret-size", marker: "secret-size"},
		{name: "excessive edit body limit", key: "GATEWAY_IMAGE_EDITS_MAX_REQUEST_BODY_BYTES", value: "268435457", marker: ""},
		{name: "invalid spool limit", key: "GATEWAY_IMAGE_EDIT_MAX_CONCURRENT_SPOOLS", value: "secret-limit", marker: "secret-limit"},
		{name: "excessive spool limit", key: "GATEWAY_IMAGE_EDIT_MAX_CONCURRENT_SPOOLS", value: "129", marker: ""},
		{name: "invalid billing mode", key: "GATEWAY_BILLING_MODE", value: "secret-mode", marker: "secret-mode"},
		{name: "invalid margin", key: "GATEWAY_MINIMUM_MARGIN_BPS", value: "secret-margin", marker: "secret-margin"},
		{name: "excessive margin", key: "GATEWAY_MINIMUM_MARGIN_BPS", value: "10001", marker: ""},
		{name: "invalid replay limit", key: "GATEWAY_IDEMPOTENCY_MAX_RESPONSE_BYTES", value: "secret-size", marker: "secret-size"},
		{name: "excessive replay limit", key: "GATEWAY_IDEMPOTENCY_MAX_RESPONSE_BYTES", value: "268435457", marker: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(func(key string) (string, bool) {
				if key == tt.key {
					return tt.value, true
				}
				if key == "GATEWAY_DATABASE_URL" {
					return "postgres://gateway:test@localhost/gateway", true
				}
				return "", false
			})
			if err == nil {
				t.Fatal("Load() error = nil, want error")
			}
			if tt.marker != "" && strings.Contains(err.Error(), tt.marker) {
				t.Fatalf("error leaked invalid value %q: %v", tt.marker, err)
			}
		})
	}
}

func TestLoadRequiresDatabaseURLWithoutLeakingIt(t *testing.T) {
	t.Parallel()
	_, err := Load(func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "GATEWAY_DATABASE_URL") {
		t.Fatalf("Load() error = %v", err)
	}
}
