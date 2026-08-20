// Package config loads and validates gateway process configuration.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPAddr        = ":8080"
	defaultLogLevel        = "info"
	defaultShutdownTimeout = 10 * time.Second
	defaultGoogleTimeout   = 2 * time.Minute
	maxGoogleTimeout       = 10 * time.Minute
	defaultGeminiBodyBytes = int64(32 * 1024 * 1024)
)

// LookupEnv matches os.LookupEnv and makes environment loading testable.
type LookupEnv func(string) (string, bool)

// Config contains non-provider process settings. Provider credentials remain
// in their opaque registry and are never exposed through this structure.
type Config struct {
	HTTPAddr        string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	DatabaseURL     string
	GoogleTimeout   time.Duration
	GeminiBodyBytes int64
}

// Load reads configuration through lookup and validates every value before
// the server starts. Errors name the setting but never echo its value.
func Load(lookup LookupEnv) (Config, error) {
	cfg := Config{
		HTTPAddr:        defaultHTTPAddr,
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: defaultShutdownTimeout,
		GoogleTimeout:   defaultGoogleTimeout,
		GeminiBodyBytes: defaultGeminiBodyBytes,
	}

	if value, ok := lookup("GATEWAY_HTTP_ADDR"); ok {
		cfg.HTTPAddr = strings.TrimSpace(value)
	}
	if value, ok := lookup("GATEWAY_LOG_LEVEL"); ok {
		level, err := parseLogLevel(value)
		if err != nil {
			return Config{}, fmt.Errorf("GATEWAY_LOG_LEVEL: %w", err)
		}
		cfg.LogLevel = level
	}
	if value, ok := lookup("GATEWAY_SHUTDOWN_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 {
			return Config{}, fmt.Errorf("GATEWAY_SHUTDOWN_TIMEOUT: must be a positive duration")
		}
		cfg.ShutdownTimeout = duration
	}
	if value, ok := lookup("GATEWAY_DATABASE_URL"); ok {
		cfg.DatabaseURL = strings.TrimSpace(value)
	}
	if value, ok := lookup("GATEWAY_GOOGLE_REQUEST_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > maxGoogleTimeout {
			return Config{}, fmt.Errorf("GATEWAY_GOOGLE_REQUEST_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.GoogleTimeout = duration
	}
	if value, ok := lookup("GATEWAY_GEMINI_MAX_REQUEST_BODY_BYTES"); ok {
		bodyBytes, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || bodyBytes <= 0 || bodyBytes > defaultGeminiBodyBytes {
			return Config{}, fmt.Errorf("GATEWAY_GEMINI_MAX_REQUEST_BODY_BYTES: must be an integer between 1 and 33554432")
		}
		cfg.GeminiBodyBytes = bodyBytes
	}

	if err := validateHTTPAddr(cfg.HTTPAddr); err != nil {
		return Config{}, fmt.Errorf("GATEWAY_HTTP_ADDR: %w", err)
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("GATEWAY_DATABASE_URL: must not be empty")
	}

	return cfg, nil
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("must be one of debug, info, warn, or error")
	}
}

func validateHTTPAddr(addr string) error {
	if addr == "" {
		return fmt.Errorf("must not be empty")
	}

	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be a host:port listen address")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("must contain a valid numeric port")
	}

	return nil
}
