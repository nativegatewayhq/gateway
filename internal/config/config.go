// Package config loads and validates gateway process configuration.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/clientip"
	"github.com/nativegatewayhq/gateway/internal/imagestorage"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
)

const (
	defaultHTTPAddr            = ":8080"
	defaultLogLevel            = "info"
	defaultShutdownTimeout     = 10 * time.Second
	defaultGoogleTimeout       = 2 * time.Minute
	maxGoogleTimeout           = 10 * time.Minute
	defaultGeminiBodyBytes     = int64(32 * 1024 * 1024)
	defaultImagesTimeout       = 2 * time.Minute
	maxImagesTimeout           = 10 * time.Minute
	defaultImagesBodyBytes     = int64(1024 * 1024)
	defaultImageEditsBodyBytes = int64(64 * 1024 * 1024)
	defaultReplayBodyBytes     = int64(32 * 1024 * 1024)
	defaultReconcileInterval   = 5 * time.Second
	defaultReconcileLease      = 30 * time.Second
	defaultReconcileBackoff    = 5 * time.Second
	defaultReconcileMaxBackoff = time.Hour
	defaultRateLimitTimeout    = 100 * time.Millisecond
)

// LookupEnv matches os.LookupEnv and makes environment loading testable.
type LookupEnv func(string) (string, bool)

type BillingMode string
type RateLimitMode string
type ProviderHealthMode string

const (
	BillingDisabled        BillingMode        = "disabled"
	BillingRequired        BillingMode        = "required"
	RateLimitDisabled      RateLimitMode      = "disabled"
	RateLimitRequired      RateLimitMode      = "required"
	ProviderHealthDisabled ProviderHealthMode = "disabled"
	ProviderHealthRequired ProviderHealthMode = "required"
)

// Config contains non-provider process settings. Provider credentials remain
// in their opaque registry and are never exposed through this structure.
type Config struct {
	HTTPAddr             string
	LogLevel             slog.Level
	ShutdownTimeout      time.Duration
	DatabaseURL          string
	GoogleTimeout        time.Duration
	GeminiBodyBytes      int64
	ImagesTimeout        time.Duration
	ImagesBodyBytes      int64
	ImageEditsBodyBytes  int64
	ImageEditSpoolLimit  int
	BillingMode          BillingMode
	MinimumMarginBPS     int64
	ReplayBodyBytes      int64
	ReconcileInterval    time.Duration
	ReconcileLease       time.Duration
	ReconcileBackoff     time.Duration
	ReconcileMaxBackoff  time.Duration
	ReconcileBatchSize   int
	ReconcileMaxAttempts int
	RateLimitMode        RateLimitMode
	RedisURL             string
	RateLimitTimeout     time.Duration
	ProviderHealthMode   ProviderHealthMode
	ProviderHealth       providerhealth.Config
	ImageStorage         imagestorage.Config
	TrustedProxyPrefixes []netip.Prefix
}

// Load reads configuration through lookup and validates every value before
// the server starts. Errors name the setting but never echo its value.
func Load(lookup LookupEnv) (Config, error) {
	cfg := Config{
		HTTPAddr:             defaultHTTPAddr,
		LogLevel:             slog.LevelInfo,
		ShutdownTimeout:      defaultShutdownTimeout,
		GoogleTimeout:        defaultGoogleTimeout,
		GeminiBodyBytes:      defaultGeminiBodyBytes,
		ImagesTimeout:        defaultImagesTimeout,
		ImagesBodyBytes:      defaultImagesBodyBytes,
		ImageEditsBodyBytes:  defaultImageEditsBodyBytes,
		ImageEditSpoolLimit:  8,
		BillingMode:          BillingDisabled,
		ReplayBodyBytes:      defaultReplayBodyBytes,
		ReconcileInterval:    defaultReconcileInterval,
		ReconcileLease:       defaultReconcileLease,
		ReconcileBackoff:     defaultReconcileBackoff,
		ReconcileMaxBackoff:  defaultReconcileMaxBackoff,
		ReconcileBatchSize:   10,
		ReconcileMaxAttempts: 5,
		RateLimitMode:        RateLimitDisabled,
		RateLimitTimeout:     defaultRateLimitTimeout,
		ProviderHealthMode:   ProviderHealthDisabled,
		ProviderHealth:       providerhealth.DefaultConfig(),
		ImageStorage:         imagestorage.DefaultConfig(),
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
	if value, ok := lookup("GATEWAY_OPENAI_IMAGES_REQUEST_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > maxImagesTimeout {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_IMAGES_REQUEST_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.ImagesTimeout = duration
	}
	if value, ok := lookup("GATEWAY_OPENAI_IMAGES_MAX_REQUEST_BODY_BYTES"); ok {
		bodyBytes, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || bodyBytes <= 0 || bodyBytes > defaultImagesBodyBytes {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_IMAGES_MAX_REQUEST_BODY_BYTES: must be an integer between 1 and 1048576")
		}
		cfg.ImagesBodyBytes = bodyBytes
	}
	if value, ok := lookup("GATEWAY_IMAGE_EDITS_MAX_REQUEST_BODY_BYTES"); ok {
		bodyBytes, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || bodyBytes <= 0 || bodyBytes > 256*1024*1024 {
			return Config{}, fmt.Errorf("GATEWAY_IMAGE_EDITS_MAX_REQUEST_BODY_BYTES: must be an integer between 1 and 268435456")
		}
		cfg.ImageEditsBodyBytes = bodyBytes
	}
	if value, ok := lookup("GATEWAY_IMAGE_EDIT_MAX_CONCURRENT_SPOOLS"); ok {
		limit, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || limit < 1 || limit > 128 {
			return Config{}, fmt.Errorf("GATEWAY_IMAGE_EDIT_MAX_CONCURRENT_SPOOLS: must be an integer between 1 and 128")
		}
		cfg.ImageEditSpoolLimit = limit
	}
	if value, ok := lookup("GATEWAY_BILLING_MODE"); ok {
		switch BillingMode(strings.ToLower(strings.TrimSpace(value))) {
		case BillingDisabled:
			cfg.BillingMode = BillingDisabled
		case BillingRequired:
			cfg.BillingMode = BillingRequired
		default:
			return Config{}, fmt.Errorf("GATEWAY_BILLING_MODE: must be disabled or required")
		}
	}
	if value, ok := lookup("GATEWAY_MINIMUM_MARGIN_BPS"); ok {
		margin, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || margin < 0 || margin > 10_000 {
			return Config{}, fmt.Errorf("GATEWAY_MINIMUM_MARGIN_BPS: must be an integer between 0 and 10000")
		}
		cfg.MinimumMarginBPS = margin
	}
	if value, ok := lookup("GATEWAY_IDEMPOTENCY_MAX_RESPONSE_BYTES"); ok {
		limit, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || limit < 1 || limit > 256*1024*1024 {
			return Config{}, fmt.Errorf("GATEWAY_IDEMPOTENCY_MAX_RESPONSE_BYTES: must be an integer between 1 and 268435456")
		}
		cfg.ReplayBodyBytes = limit
	}
	if err := loadReconciliation(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if value, ok := lookup("GATEWAY_RATE_LIMIT_MODE"); ok {
		switch RateLimitMode(strings.ToLower(strings.TrimSpace(value))) {
		case RateLimitDisabled:
			cfg.RateLimitMode = RateLimitDisabled
		case RateLimitRequired:
			cfg.RateLimitMode = RateLimitRequired
		default:
			return Config{}, fmt.Errorf("GATEWAY_RATE_LIMIT_MODE: must be disabled or required")
		}
	}
	if value, ok := lookup("GATEWAY_REDIS_URL"); ok {
		cfg.RedisURL = strings.TrimSpace(value)
	}
	if value, ok := lookup("GATEWAY_RATE_LIMIT_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > time.Second {
			return Config{}, fmt.Errorf("GATEWAY_RATE_LIMIT_TIMEOUT: must be a positive duration no greater than 1s")
		}
		cfg.RateLimitTimeout = duration
	}
	if err := loadProviderHealth(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := loadImageStorage(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if value, ok := lookup("GATEWAY_TRUSTED_PROXY_CIDRS"); ok {
		parts := strings.Split(value, ",")
		if len(parts) > 128 {
			return Config{}, fmt.Errorf("GATEWAY_TRUSTED_PROXY_CIDRS: must contain no more than 128 prefixes")
		}
		prefixes := make([]netip.Prefix, 0, len(parts))
		for _, part := range parts {
			prefix, err := netip.ParsePrefix(strings.TrimSpace(part))
			if err != nil {
				return Config{}, fmt.Errorf("GATEWAY_TRUSTED_PROXY_CIDRS: must contain valid comma-separated CIDR prefixes")
			}
			prefixes = append(prefixes, prefix)
		}
		canonical, err := clientip.CanonicalPrefixes(prefixes, 128)
		if err != nil || len(canonical) != len(prefixes) {
			return Config{}, fmt.Errorf("GATEWAY_TRUSTED_PROXY_CIDRS: invalid trusted proxy policy")
		}
		cfg.TrustedProxyPrefixes = canonical
	}

	if err := validateHTTPAddr(cfg.HTTPAddr); err != nil {
		return Config{}, fmt.Errorf("GATEWAY_HTTP_ADDR: %w", err)
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("GATEWAY_DATABASE_URL: must not be empty")
	}
	if cfg.RateLimitMode == RateLimitRequired && cfg.RedisURL == "" {
		return Config{}, fmt.Errorf("GATEWAY_REDIS_URL: must not be empty when rate limiting is required")
	}
	if cfg.ProviderHealthMode == ProviderHealthRequired && cfg.RedisURL == "" {
		return Config{}, fmt.Errorf("GATEWAY_REDIS_URL: must not be empty when provider health is required")
	}
	if cfg.RedisURL != "" {
		parsed, err := url.Parse(cfg.RedisURL)
		if err != nil || (parsed.Scheme != "redis" && parsed.Scheme != "rediss") || parsed.Host == "" {
			return Config{}, fmt.Errorf("GATEWAY_REDIS_URL: must be a valid redis or rediss URL")
		}
	}

	return cfg, nil
}

func loadImageStorage(cfg *Config, lookup LookupEnv) error {
	stringsSettings := []struct {
		key    string
		target *string
	}{
		{"GATEWAY_IMAGE_STORAGE_ENDPOINT", &cfg.ImageStorage.Endpoint},
		{"GATEWAY_IMAGE_STORAGE_REGION", &cfg.ImageStorage.Region},
		{"GATEWAY_IMAGE_STORAGE_BUCKET", &cfg.ImageStorage.Bucket},
		{"GATEWAY_IMAGE_STORAGE_ACCESS_KEY_ID", &cfg.ImageStorage.AccessKeyID},
		{"GATEWAY_IMAGE_STORAGE_SECRET_ACCESS_KEY", &cfg.ImageStorage.SecretAccessKey},
		{"GATEWAY_IMAGE_STORAGE_CDN_BASE_URL", &cfg.ImageStorage.CDNBaseURL},
		{"GATEWAY_IMAGE_STORAGE_TEMP_DIR", &cfg.ImageStorage.TemporaryDirectory},
	}
	if value, ok := lookup("GATEWAY_IMAGE_STORAGE_MODE"); ok {
		cfg.ImageStorage.Mode = imagestorage.Mode(strings.ToLower(strings.TrimSpace(value)))
	}
	for _, setting := range stringsSettings {
		if value, ok := lookup(setting.key); ok {
			*setting.target = strings.TrimSpace(value)
		}
	}
	integerSettings := []struct {
		key    string
		target *int64
	}{
		{"GATEWAY_IMAGE_STORAGE_MAX_IMAGE_BYTES", &cfg.ImageStorage.MaximumImageBytes},
		{"GATEWAY_IMAGE_STORAGE_MAX_TOTAL_BYTES", &cfg.ImageStorage.MaximumTotalBytes},
	}
	for _, setting := range integerSettings {
		if value, ok := lookup(setting.key); ok {
			parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return fmt.Errorf("%s: must be a valid bounded integer", setting.key)
			}
			*setting.target = parsed
		}
	}
	if value, ok := lookup("GATEWAY_IMAGE_STORAGE_MAX_IMAGES"); ok {
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("GATEWAY_IMAGE_STORAGE_MAX_IMAGES: must be a valid bounded integer")
		}
		cfg.ImageStorage.MaximumImages = parsed
	}
	durations := []struct {
		key    string
		target *time.Duration
	}{
		{"GATEWAY_IMAGE_STORAGE_FETCH_TIMEOUT", &cfg.ImageStorage.FetchTimeout},
		{"GATEWAY_IMAGE_STORAGE_UPLOAD_TIMEOUT", &cfg.ImageStorage.UploadTimeout},
	}
	for _, setting := range durations {
		if value, ok := lookup(setting.key); ok {
			parsed, err := time.ParseDuration(strings.TrimSpace(value))
			if err != nil {
				return fmt.Errorf("%s: must be a valid bounded duration", setting.key)
			}
			*setting.target = parsed
		}
	}
	if err := cfg.ImageStorage.Validate(); err != nil {
		return fmt.Errorf("GATEWAY_IMAGE_STORAGE_*: settings are invalid")
	}
	return nil
}

func loadProviderHealth(cfg *Config, lookup LookupEnv) error {
	if value, ok := lookup("GATEWAY_PROVIDER_HEALTH_MODE"); ok {
		switch ProviderHealthMode(strings.ToLower(strings.TrimSpace(value))) {
		case ProviderHealthDisabled:
			cfg.ProviderHealthMode = ProviderHealthDisabled
		case ProviderHealthRequired:
			cfg.ProviderHealthMode = ProviderHealthRequired
		default:
			return fmt.Errorf("GATEWAY_PROVIDER_HEALTH_MODE: must be disabled or required")
		}
	}
	durations := []struct {
		key    string
		target *time.Duration
	}{
		{"GATEWAY_PROVIDER_HEALTH_WINDOW", &cfg.ProviderHealth.Window},
		{"GATEWAY_PROVIDER_HEALTH_BUCKET", &cfg.ProviderHealth.Bucket},
		{"GATEWAY_PROVIDER_HEALTH_OPEN_DURATION", &cfg.ProviderHealth.OpenDuration},
		{"GATEWAY_PROVIDER_HEALTH_MAXIMUM_OPEN_DURATION", &cfg.ProviderHealth.MaximumOpenDuration},
		{"GATEWAY_PROVIDER_HEALTH_PROBE_LEASE", &cfg.ProviderHealth.ProbeLease},
		{"GATEWAY_PROVIDER_HEALTH_COMMAND_TIMEOUT", &cfg.ProviderHealth.CommandTimeout},
	}
	for _, setting := range durations {
		if value, ok := lookup(setting.key); ok {
			duration, err := time.ParseDuration(strings.TrimSpace(value))
			if err != nil {
				return fmt.Errorf("%s: must be a valid bounded duration", setting.key)
			}
			*setting.target = duration
		}
	}
	integers := []struct {
		key    string
		target *int64
	}{
		{"GATEWAY_PROVIDER_HEALTH_MINIMUM_SAMPLES", &cfg.ProviderHealth.MinimumSamples},
		{"GATEWAY_PROVIDER_HEALTH_FAILURE_THRESHOLD_BPS", &cfg.ProviderHealth.FailureThresholdBPS},
	}
	for _, setting := range integers {
		if value, ok := lookup(setting.key); ok {
			parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return fmt.Errorf("%s: must be a valid bounded integer", setting.key)
			}
			*setting.target = parsed
		}
	}
	if value, ok := lookup("GATEWAY_PROVIDER_HEALTH_KEY_PREFIX"); ok {
		cfg.ProviderHealth.KeyPrefix = strings.TrimSpace(value)
	}
	if !cfg.ProviderHealth.Valid() {
		return fmt.Errorf("GATEWAY_PROVIDER_HEALTH_*: settings are outside allowed bounds")
	}
	return nil
}

func loadReconciliation(cfg *Config, lookup LookupEnv) error {
	durations := []struct {
		key     string
		target  *time.Duration
		maximum time.Duration
	}{
		{"GATEWAY_RECONCILIATION_INTERVAL", &cfg.ReconcileInterval, time.Minute},
		{"GATEWAY_RECONCILIATION_LEASE", &cfg.ReconcileLease, 10 * time.Minute},
		{"GATEWAY_RECONCILIATION_BASE_BACKOFF", &cfg.ReconcileBackoff, time.Hour},
		{"GATEWAY_RECONCILIATION_MAX_BACKOFF", &cfg.ReconcileMaxBackoff, 24 * time.Hour},
	}
	for _, setting := range durations {
		if value, ok := lookup(setting.key); ok {
			duration, err := time.ParseDuration(strings.TrimSpace(value))
			if err != nil || duration <= 0 || duration > setting.maximum {
				return fmt.Errorf("%s: must be a positive duration within its supported limit", setting.key)
			}
			*setting.target = duration
		}
	}
	if cfg.ReconcileMaxBackoff < cfg.ReconcileBackoff {
		return fmt.Errorf("GATEWAY_RECONCILIATION_MAX_BACKOFF: must not be less than base backoff")
	}
	integers := []struct {
		key     string
		target  *int
		maximum int
	}{
		{"GATEWAY_RECONCILIATION_BATCH_SIZE", &cfg.ReconcileBatchSize, 100},
		{"GATEWAY_RECONCILIATION_MAX_ATTEMPTS", &cfg.ReconcileMaxAttempts, 100},
	}
	for _, setting := range integers {
		if value, ok := lookup(setting.key); ok {
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || parsed < 1 || parsed > setting.maximum {
				return fmt.Errorf("%s: must be an integer between 1 and %d", setting.key, setting.maximum)
			}
			*setting.target = parsed
		}
	}
	return nil
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
