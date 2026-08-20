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
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"GATEWAY_HTTP_ADDR":        "127.0.0.1:9090",
		"GATEWAY_LOG_LEVEL":        "debug",
		"GATEWAY_SHUTDOWN_TIMEOUT": "3s",
		"GATEWAY_DATABASE_URL":     "postgres://gateway:test@localhost/gateway",
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
