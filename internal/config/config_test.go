package config

import (
	"encoding/base64"
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
	if cfg.ReconcileInterval != 5*time.Second || cfg.ReconcileLease != 30*time.Second || cfg.ReconcileBackoff != 5*time.Second || cfg.ReconcileMaxBackoff != time.Hour || cfg.ReconcileBatchSize != 10 || cfg.ReconcileMaxAttempts != 5 {
		t.Errorf("reconciliation config = %+v", cfg)
	}
	if cfg.RateLimitMode != RateLimitDisabled || cfg.RedisURL != "" || cfg.RateLimitTimeout != 100*time.Millisecond {
		t.Errorf("rate limit config = %+v", cfg)
	}
	if cfg.ProviderHealthMode != ProviderHealthDisabled || !cfg.ProviderHealth.Valid() || cfg.ProviderHealth.Window != time.Minute || cfg.ProviderHealth.FailureThresholdBPS != 5_000 {
		t.Errorf("provider health config = %+v", cfg)
	}
	if cfg.ImageStorage.Mode != "provider" || cfg.ImageStorage.MaximumImages != 10 || cfg.ImageStorage.MaximumImageBytes != 32<<20 {
		t.Errorf("image storage config = %+v", cfg.ImageStorage)
	}
	if cfg.Telemetry.Mode != "disabled" || cfg.Telemetry.ServiceName != "native-ai-gateway" || cfg.Telemetry.SampleRatio != 0.1 {
		t.Errorf("telemetry config=%+v", cfg.Telemetry)
	}
	if cfg.ReplicateEnabled || cfg.ReplicateEndpoint != "https://api.replicate.com" || cfg.ReplicateTimeout != 2*time.Minute || cfg.ReplicateBodyBytes != 1<<20 {
		t.Errorf("replicate config=%+v", cfg)
	}
}

func TestLoadReplicateConfigurationAndSecretSafeFailures(t *testing.T) {
	values := map[string]string{"GATEWAY_DATABASE_URL": "postgres://gateway:test@localhost/gateway", "GATEWAY_REPLICATE_API_TOKEN": "provider-secret", "GATEWAY_REPLICATE_API_ENDPOINT": "https://api.replicate.example", "GATEWAY_PUBLIC_BASE_URL": "https://gateway.example", "GATEWAY_REPLICATE_MODELS": "owner/model:version-a, owner/model:version-b", "GATEWAY_REPLICATE_REQUEST_TIMEOUT": "90s", "GATEWAY_REPLICATE_MAX_BODY_BYTES": "2097152"}
	cfg, err := Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ReplicateEnabled || len(cfg.ReplicateModels) != 2 || cfg.ReplicateTimeout != 90*time.Second || cfg.ReplicateBodyBytes != 2097152 {
		t.Fatalf("config=%+v", cfg)
	}
	delete(values, "GATEWAY_PUBLIC_BASE_URL")
	_, err = Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err == nil || strings.Contains(err.Error(), "provider-secret") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadReplicateRequiredWebhookConfiguration(t *testing.T) {
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	callbackSecret := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	values := map[string]string{
		"GATEWAY_DATABASE_URL":                      "postgres://gateway:test@localhost/gateway",
		"GATEWAY_REPLICATE_API_TOKEN":               "provider-secret",
		"GATEWAY_REPLICATE_MODELS":                  "owner/model:version",
		"GATEWAY_PUBLIC_BASE_URL":                   "https://gateway.example",
		"GATEWAY_REPLICATE_WEBHOOK_MODE":            "required",
		"GATEWAY_REPLICATE_WEBHOOK_SIGNING_SECRETS": secret,
		"GATEWAY_REPLICATE_WEBHOOK_CALLBACK_SECRET": callbackSecret,
		"GATEWAY_REPLICATE_WEBHOOK_TOLERANCE":       "4m",
		"GATEWAY_REPLICATE_WEBHOOK_BINDING_TTL":     "168h",
	}
	cfg, err := Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReplicateWebhookMode != ReplicateWebhookRequired || len(cfg.ReplicateWebhookSecrets) != 1 || len(cfg.ReplicateWebhookCallbackSecret) != 32 || cfg.ReplicateWebhookTolerance != 4*time.Minute || cfg.ReplicateWebhookBindingTTL != 7*24*time.Hour {
		t.Fatalf("config=%+v", cfg)
	}
	values["GATEWAY_PUBLIC_BASE_URL"] = "http://localhost:8080"
	if _, err := Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil {
		t.Fatal("required webhook accepted insecure public base")
	}
	values["GATEWAY_PUBLIC_BASE_URL"] = "https://gateway.example"
	values["GATEWAY_REPLICATE_WEBHOOK_SIGNING_SECRETS"] = "whsec_provider-secret"
	if _, err := Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil || strings.Contains(err.Error(), "provider-secret") {
		t.Fatalf("secret-safe error=%v", err)
	}
}

func TestLoadFalRequiredWebhookConfiguration(t *testing.T) {
	callbackSecret := base64.StdEncoding.EncodeToString([]byte("abcdef0123456789abcdef0123456789"))
	values := map[string]string{
		"GATEWAY_DATABASE_URL":                "postgres://gateway:test@localhost/gateway",
		"GATEWAY_FAL_API_KEY":                 "provider-secret",
		"GATEWAY_FAL_MODELS":                  "fal-ai/flux/dev",
		"GATEWAY_PUBLIC_BASE_URL":             "https://gateway.example",
		"GATEWAY_FAL_WEBHOOK_MODE":            "required",
		"GATEWAY_FAL_WEBHOOK_CALLBACK_SECRET": callbackSecret,
		"GATEWAY_FAL_WEBHOOK_BINDING_TTL":     "24h",
		"GATEWAY_FAL_JWKS_CACHE_TTL":          "12h",
		"GATEWAY_FAL_JWKS_REFRESH_COOLDOWN":   "30s",
	}
	cfg, err := Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FalWebhookMode != FalWebhookRequired || len(cfg.FalWebhookCallbackSecret) != 32 || cfg.FalWebhookBindingTTL != 24*time.Hour || cfg.FalJWKSCacheTTL != 12*time.Hour || cfg.FalJWKSURL != defaultFalJWKSURL {
		t.Fatalf("config=%+v", cfg)
	}
	values["GATEWAY_PUBLIC_BASE_URL"] = "http://localhost:8080"
	if _, err := Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil {
		t.Fatal("required fal webhook accepted insecure public base")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"GATEWAY_HTTP_ADDR":                             "127.0.0.1:9090",
		"GATEWAY_LOG_LEVEL":                             "debug",
		"GATEWAY_SHUTDOWN_TIMEOUT":                      "3s",
		"GATEWAY_DATABASE_URL":                          "postgres://gateway:test@localhost/gateway",
		"GATEWAY_GOOGLE_REQUEST_TIMEOUT":                "90s",
		"GATEWAY_GEMINI_MAX_REQUEST_BODY_BYTES":         "1048576",
		"GATEWAY_OPENAI_IMAGES_REQUEST_TIMEOUT":         "80s",
		"GATEWAY_OPENAI_IMAGES_MAX_REQUEST_BODY_BYTES":  "524288",
		"GATEWAY_IMAGE_EDITS_MAX_REQUEST_BODY_BYTES":    "33554432",
		"GATEWAY_IMAGE_EDIT_MAX_CONCURRENT_SPOOLS":      "4",
		"GATEWAY_BILLING_MODE":                          "required",
		"GATEWAY_MINIMUM_MARGIN_BPS":                    "1250",
		"GATEWAY_IDEMPOTENCY_MAX_RESPONSE_BYTES":        "134217728",
		"GATEWAY_RECONCILIATION_INTERVAL":               "2s",
		"GATEWAY_RECONCILIATION_LEASE":                  "20s",
		"GATEWAY_RECONCILIATION_BASE_BACKOFF":           "3s",
		"GATEWAY_RECONCILIATION_MAX_BACKOFF":            "30m",
		"GATEWAY_RECONCILIATION_BATCH_SIZE":             "20",
		"GATEWAY_RECONCILIATION_MAX_ATTEMPTS":           "7",
		"GATEWAY_RATE_LIMIT_MODE":                       "required",
		"GATEWAY_REDIS_URL":                             "rediss://user:secret@redis.example:6380/1",
		"GATEWAY_RATE_LIMIT_TIMEOUT":                    "250ms",
		"GATEWAY_PROVIDER_HEALTH_MODE":                  "required",
		"GATEWAY_PROVIDER_HEALTH_WINDOW":                "2m",
		"GATEWAY_PROVIDER_HEALTH_BUCKET":                "5s",
		"GATEWAY_PROVIDER_HEALTH_MINIMUM_SAMPLES":       "20",
		"GATEWAY_PROVIDER_HEALTH_FAILURE_THRESHOLD_BPS": "6000",
		"GATEWAY_PROVIDER_HEALTH_OPEN_DURATION":         "20s",
		"GATEWAY_PROVIDER_HEALTH_MAXIMUM_OPEN_DURATION": "2m",
		"GATEWAY_PROVIDER_HEALTH_PROBE_LEASE":           "5s",
		"GATEWAY_PROVIDER_HEALTH_COMMAND_TIMEOUT":       "300ms",
		"GATEWAY_PROVIDER_HEALTH_KEY_PREFIX":            "gateway:test:health",
		"GATEWAY_IMAGE_STORAGE_MODE":                    "managed",
		"GATEWAY_IMAGE_STORAGE_ENDPOINT":                "https://account.r2.cloudflarestorage.com",
		"GATEWAY_IMAGE_STORAGE_REGION":                  "auto",
		"GATEWAY_IMAGE_STORAGE_BUCKET":                  "gateway-images",
		"GATEWAY_IMAGE_STORAGE_ACCESS_KEY_ID":           "storage-access",
		"GATEWAY_IMAGE_STORAGE_SECRET_ACCESS_KEY":       "storage-secret",
		"GATEWAY_IMAGE_STORAGE_CDN_BASE_URL":            "https://images.example.com",
		"GATEWAY_IMAGE_STORAGE_MAX_IMAGES":              "8",
		"GATEWAY_IMAGE_STORAGE_MAX_IMAGE_BYTES":         "16777216",
		"GATEWAY_IMAGE_STORAGE_MAX_TOTAL_BYTES":         "67108864",
		"GATEWAY_IMAGE_STORAGE_FETCH_TIMEOUT":           "20s",
		"GATEWAY_IMAGE_STORAGE_UPLOAD_TIMEOUT":          "45s",
		"GATEWAY_IMAGE_STORAGE_FETCH_ORIGINS_OPENAI":    "https://images.openai.com,https://cdn.openai.com",
		"GATEWAY_TELEMETRY_MODE":                        "required",
		"GATEWAY_TELEMETRY_OTLP_ENDPOINT":               "https://otel.example.com/collector",
		"GATEWAY_TELEMETRY_OTLP_AUTHORIZATION":          "Bearer telemetry-secret",
		"GATEWAY_TELEMETRY_SERVICE_NAME":                "gateway-test",
		"GATEWAY_TELEMETRY_SERVICE_VERSION":             "v1.2.3",
		"GATEWAY_TELEMETRY_ENVIRONMENT":                 "test",
		"GATEWAY_TELEMETRY_SAMPLE_RATIO":                "0.25",
		"GATEWAY_TELEMETRY_EXPORT_INTERVAL":             "15s",
		"GATEWAY_TELEMETRY_EXPORT_TIMEOUT":              "3s",
		"GATEWAY_TELEMETRY_SHUTDOWN_TIMEOUT":            "2s",
		"GATEWAY_TRUSTED_PROXY_CIDRS":                   "10.0.0.8/8, 2001:db8::1/32",
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
	if cfg.ReplayBodyBytes != 134217728 {
		t.Errorf("ReplayBodyBytes = %d", cfg.ReplayBodyBytes)
	}
	if cfg.ReconcileInterval != 2*time.Second || cfg.ReconcileLease != 20*time.Second || cfg.ReconcileBackoff != 3*time.Second || cfg.ReconcileMaxBackoff != 30*time.Minute || cfg.ReconcileBatchSize != 20 || cfg.ReconcileMaxAttempts != 7 {
		t.Errorf("reconciliation overrides = %+v", cfg)
	}
	if cfg.RateLimitMode != RateLimitRequired || cfg.RedisURL != values["GATEWAY_REDIS_URL"] || cfg.RateLimitTimeout != 250*time.Millisecond {
		t.Errorf("rate limit overrides=%+v", cfg)
	}
	if cfg.ProviderHealthMode != ProviderHealthRequired || cfg.ProviderHealth.Window != 2*time.Minute || cfg.ProviderHealth.Bucket != 5*time.Second || cfg.ProviderHealth.MinimumSamples != 20 || cfg.ProviderHealth.FailureThresholdBPS != 6_000 || cfg.ProviderHealth.OpenDuration != 20*time.Second || cfg.ProviderHealth.MaximumOpenDuration != 2*time.Minute || cfg.ProviderHealth.ProbeLease != 5*time.Second || cfg.ProviderHealth.CommandTimeout != 300*time.Millisecond || cfg.ProviderHealth.KeyPrefix != "gateway:test:health" {
		t.Errorf("provider health overrides=%+v", cfg.ProviderHealth)
	}
	if cfg.ImageStorage.Mode != "managed" || cfg.ImageStorage.Bucket != "gateway-images" || cfg.ImageStorage.MaximumImages != 8 || cfg.ImageStorage.MaximumImageBytes != 16777216 || cfg.ImageStorage.FetchTimeout != 20*time.Second || cfg.ImageStorage.UploadTimeout != 45*time.Second {
		t.Errorf("image storage overrides=%+v", cfg.ImageStorage)
	}
	if len(cfg.ImageStorage.FetchOrigins["openai"]) != 2 {
		t.Errorf("fetch origins=%v", cfg.ImageStorage.FetchOrigins)
	}
	if cfg.Telemetry.Mode != "required" || cfg.Telemetry.Endpoint != "https://otel.example.com/collector" || cfg.Telemetry.SampleRatio != 0.25 || cfg.Telemetry.ExportInterval != 15*time.Second || cfg.Telemetry.ShutdownTimeout != 2*time.Second {
		t.Errorf("telemetry overrides=%+v", cfg.Telemetry)
	}
	if len(cfg.TrustedProxyPrefixes) != 2 || cfg.TrustedProxyPrefixes[0].String() != "10.0.0.0/8" || cfg.TrustedProxyPrefixes[1].String() != "2001:db8::/32" {
		t.Errorf("trusted proxies=%v", cfg.TrustedProxyPrefixes)
	}
}

func TestLoadRejectsInvalidTrustedProxyWithoutEcho(t *testing.T) {
	value := "secret-invalid-prefix"
	_, err := Load(func(key string) (string, bool) {
		if key == "GATEWAY_DATABASE_URL" {
			return "postgres://gateway:test@localhost/gateway", true
		}
		if key == "GATEWAY_TRUSTED_PROXY_CIDRS" {
			return value, true
		}
		return "", false
	})
	if err == nil || strings.Contains(err.Error(), value) {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadRejectsInvalidManagedStorageWithoutEchoingSecrets(t *testing.T) {
	secret := "storage-secret-marker"
	_, err := Load(func(key string) (string, bool) {
		values := map[string]string{
			"GATEWAY_DATABASE_URL":                    "postgres://gateway:test@localhost/gateway",
			"GATEWAY_IMAGE_STORAGE_MODE":              "managed",
			"GATEWAY_IMAGE_STORAGE_ENDPOINT":          "http://public.example.com",
			"GATEWAY_IMAGE_STORAGE_REGION":            "auto",
			"GATEWAY_IMAGE_STORAGE_BUCKET":            "gateway-images",
			"GATEWAY_IMAGE_STORAGE_ACCESS_KEY_ID":     "access",
			"GATEWAY_IMAGE_STORAGE_SECRET_ACCESS_KEY": secret,
			"GATEWAY_IMAGE_STORAGE_CDN_BASE_URL":      "https://images.example.com",
		}
		value, ok := values[key]
		return value, ok
	})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadRejectsOverlappingTrustedProxies(t *testing.T) {
	_, err := Load(func(key string) (string, bool) {
		switch key {
		case "GATEWAY_DATABASE_URL":
			return "postgres://gateway:test@localhost/gateway", true
		case "GATEWAY_TRUSTED_PROXY_CIDRS":
			return "10.0.0.0/8,10.1.0.0/16", true
		default:
			return "", false
		}
	})
	if err == nil {
		t.Fatal("expected overlapping proxy prefixes to fail")
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
		{name: "invalid reconciliation interval", key: "GATEWAY_RECONCILIATION_INTERVAL", value: "secret-duration", marker: "secret-duration"},
		{name: "invalid reconciliation batch", key: "GATEWAY_RECONCILIATION_BATCH_SIZE", value: "secret-count", marker: "secret-count"},
		{name: "invalid reconciliation attempts", key: "GATEWAY_RECONCILIATION_MAX_ATTEMPTS", value: "101", marker: ""},
		{name: "invalid rate limit mode", key: "GATEWAY_RATE_LIMIT_MODE", value: "secret-mode", marker: "secret-mode"},
		{name: "invalid Redis URL", key: "GATEWAY_REDIS_URL", value: "http://secret-password", marker: "secret-password"},
		{name: "invalid rate limit timeout", key: "GATEWAY_RATE_LIMIT_TIMEOUT", value: "secret-timeout", marker: "secret-timeout"},
		{name: "invalid provider health mode", key: "GATEWAY_PROVIDER_HEALTH_MODE", value: "secret-health-mode", marker: "secret-health-mode"},
		{name: "invalid provider health duration", key: "GATEWAY_PROVIDER_HEALTH_WINDOW", value: "secret-health-duration", marker: "secret-health-duration"},
		{name: "invalid provider health integer", key: "GATEWAY_PROVIDER_HEALTH_MINIMUM_SAMPLES", value: "secret-health-count", marker: "secret-health-count"},
		{name: "invalid provider health prefix", key: "GATEWAY_PROVIDER_HEALTH_KEY_PREFIX", value: "secret health prefix", marker: "secret health prefix"},
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

func TestRateLimitRequiredNeedsRedisURL(t *testing.T) {
	_, err := Load(func(key string) (string, bool) {
		switch key {
		case "GATEWAY_DATABASE_URL":
			return "postgres://gateway:test@localhost/gateway", true
		case "GATEWAY_RATE_LIMIT_MODE":
			return "required", true
		}
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), "GATEWAY_REDIS_URL") {
		t.Fatalf("error=%v", err)
	}
}

func TestProviderHealthRequiredNeedsRedisURL(t *testing.T) {
	_, err := Load(func(key string) (string, bool) {
		switch key {
		case "GATEWAY_DATABASE_URL":
			return "postgres://gateway:test@localhost/gateway", true
		case "GATEWAY_PROVIDER_HEALTH_MODE":
			return "required", true
		}
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), "GATEWAY_REDIS_URL") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadRequiresDatabaseURLWithoutLeakingIt(t *testing.T) {
	t.Parallel()
	_, err := Load(func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "GATEWAY_DATABASE_URL") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadFalQueueConfiguration(t *testing.T) {
	cfg, err := Load(func(key string) (string, bool) {
		values := map[string]string{
			"GATEWAY_DATABASE_URL":       "postgres://gateway:test@localhost/gateway",
			"GATEWAY_FAL_API_KEY":        "fal-secret",
			"GATEWAY_FAL_QUEUE_ENDPOINT": "http://127.0.0.1:9090",
			"GATEWAY_FAL_MODELS":         "fal-ai/flux/dev,fal-ai/veo3",
			"GATEWAY_PUBLIC_BASE_URL":    "https://gateway.example",
		}
		value, ok := values[key]
		return value, ok
	})
	if err != nil || !cfg.FalEnabled || len(cfg.FalModels) != 2 || cfg.FalEndpoint != "http://127.0.0.1:9090" {
		t.Fatalf("config=%+v err=%v", cfg, err)
	}
}

func TestLoadRunwayConfiguration(t *testing.T) {
	values := map[string]string{"GATEWAY_DATABASE_URL": "postgres://test", "GATEWAY_RUNWAY_API_KEY": "secret", "GATEWAY_RUNWAY_MODELS": "logical", "GATEWAY_RUNWAY_MODEL_CAPABILITIES_JSON": `{"logical":{"provider_model":"gen4_turbo","text_to_video":true}}`, "GATEWAY_RUNWAY_POLL_INTERVAL": "7s"}
	cfg, err := Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RunwayEnabled || cfg.RunwayPollInterval != 7*time.Second || cfg.RunwayModelCapabilities["logical"].ProviderModel != "gen4_turbo" {
		t.Fatalf("config=%+v", cfg)
	}
	values["GATEWAY_RUNWAY_POLL_INTERVAL"] = "4s"
	if _, err = Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil {
		t.Fatal("unsafe poll interval accepted")
	}
}

func TestLoadManagedVideoStorageConfiguration(t *testing.T) {
	values := map[string]string{
		"GATEWAY_DATABASE_URL":                       "postgres://gateway:gateway@localhost/gateway",
		"GATEWAY_VIDEO_STORAGE_MODE":                 "managed",
		"GATEWAY_VIDEO_STORAGE_ENDPOINT":             "http://127.0.0.1:9000",
		"GATEWAY_VIDEO_STORAGE_REGION":               "auto",
		"GATEWAY_VIDEO_STORAGE_BUCKET":               "videos",
		"GATEWAY_VIDEO_STORAGE_ACCESS_KEY_ID":        "access",
		"GATEWAY_VIDEO_STORAGE_SECRET_ACCESS_KEY":    "secret",
		"GATEWAY_VIDEO_STORAGE_CDN_BASE_URL":         "https://cdn.example",
		"GATEWAY_VIDEO_STORAGE_FETCH_ORIGINS_RUNWAY": "https://runway.example",
	}
	cfg, err := Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil || cfg.VideoStorage.Mode != "managed" || cfg.VideoStorage.FetchOrigins["runway"][0] != "https://runway.example" {
		t.Fatalf("config=%+v err=%v", cfg.VideoStorage, err)
	}
	values["GATEWAY_VIDEO_STORAGE_FETCH_ORIGINS_RUNWAY"] = "http://runway.example"
	if _, err = Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err == nil {
		t.Fatal("unsafe video origin accepted")
	}
}

func TestOpenAISpeechConfigurationAndBillingBoundary(t *testing.T) {
	values := map[string]string{
		"GATEWAY_DATABASE_URL":                          "postgres://gateway",
		"GATEWAY_OPENAI_SPEECH_MODELS":                  "tts-1,gpt-4o-mini-tts",
		"GATEWAY_OPENAI_SPEECH_REQUEST_TIMEOUT":         "3m",
		"GATEWAY_OPENAI_SPEECH_STREAM_IDLE_TIMEOUT":     "20s",
		"GATEWAY_OPENAI_SPEECH_MAX_REQUEST_BODY_BYTES":  "65536",
		"GATEWAY_OPENAI_SPEECH_MAX_RESPONSE_BODY_BYTES": "1048576",
	}
	cfg, err := Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil || len(cfg.OpenAISpeechModels) != 2 || cfg.SpeechTimeout != 3*time.Minute || cfg.SpeechStreamIdleTimeout != 20*time.Second || cfg.SpeechRequestBytes != 65536 || cfg.SpeechResponseBytes != 1048576 {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	values["GATEWAY_BILLING_MODE"] = "required"
	if _, err = Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok }); err != nil {
		t.Fatalf("billable speech rejected: %v", err)
	}
}

func TestLoadFalRequiresModelsAndPublicOrigin(t *testing.T) {
	_, err := Load(func(key string) (string, bool) {
		if key == "GATEWAY_DATABASE_URL" {
			return "postgres://gateway:test@localhost/gateway", true
		}
		if key == "GATEWAY_FAL_API_KEY" {
			return "fal-secret", true
		}
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), "GATEWAY_PUBLIC_BASE_URL") {
		t.Fatalf("error=%v", err)
	}
}
