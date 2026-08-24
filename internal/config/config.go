// Package config loads and validates gateway process configuration.
package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/audioassets"
	"github.com/nativegatewayhq/gateway/internal/clientip"
	"github.com/nativegatewayhq/gateway/internal/imagestorage"
	"github.com/nativegatewayhq/gateway/internal/plugins"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/speechstorage"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	"github.com/nativegatewayhq/gateway/internal/videostorage"
	audiooperation "github.com/nativegatewayhq/gateway/operations/audio"
	videooperation "github.com/nativegatewayhq/gateway/operations/video"
)

const (
	defaultHTTPAddr                   = ":8080"
	defaultLogLevel                   = "info"
	defaultShutdownTimeout            = 10 * time.Second
	defaultGoogleTimeout              = 2 * time.Minute
	maxGoogleTimeout                  = 10 * time.Minute
	defaultGeminiBodyBytes            = int64(32 * 1024 * 1024)
	defaultImagesTimeout              = 2 * time.Minute
	maxImagesTimeout                  = 10 * time.Minute
	defaultImagesBodyBytes            = int64(1024 * 1024)
	defaultChatBodyBytes              = int64(8 * 1024 * 1024)
	defaultImageEditsBodyBytes        = int64(64 * 1024 * 1024)
	defaultReplayBodyBytes            = int64(32 * 1024 * 1024)
	defaultReconcileInterval          = 5 * time.Second
	defaultReconcileLease             = 30 * time.Second
	defaultReconcileBackoff           = 5 * time.Second
	defaultReconcileMaxBackoff        = time.Hour
	defaultRateLimitTimeout           = 100 * time.Millisecond
	defaultReplicateTimeout           = 2 * time.Minute
	defaultReplicateBodyBytes         = int64(1024 * 1024)
	defaultWebhookTolerance           = 5 * time.Minute
	defaultWebhookBindingTTL          = 7 * 24 * time.Hour
	defaultFalTimeout                 = 2 * time.Minute
	defaultFalBodyBytes               = int64(1024 * 1024)
	defaultFalJWKSURL                 = "https://rest.fal.ai/.well-known/jwks.json"
	defaultFalJWKSTimeout             = 5 * time.Second
	defaultFalJWKSCacheTTL            = 24 * time.Hour
	defaultFalJWKSRefresh             = time.Minute
	defaultRunwayTimeout              = 2 * time.Minute
	defaultRunwayBodyBytes            = int64(8 * 1024 * 1024)
	defaultSpeechRequestBytes         = int64(1024 * 1024)
	defaultSpeechResponseBytes        = int64(256 * 1024 * 1024)
	defaultTranscriptionRequestBytes  = int64(64 * 1024 * 1024)
	defaultTranscriptionFileBytes     = int64(60 * 1024 * 1024)
	defaultTranscriptionFieldBytes    = int64(64 * 1024)
	defaultTranscriptionResponseBytes = int64(32 * 1024 * 1024)
)

// LookupEnv matches os.LookupEnv and makes environment loading testable.
type LookupEnv func(string) (string, bool)

type BillingMode string
type RateLimitMode string
type ProviderHealthMode string
type ReplicateWebhookMode string
type FalWebhookMode string
type JobManagementMode string
type PluginMode string
type PluginRegistryMode string

const (
	BillingDisabled          BillingMode          = "disabled"
	BillingRequired          BillingMode          = "required"
	RateLimitDisabled        RateLimitMode        = "disabled"
	RateLimitRequired        RateLimitMode        = "required"
	ProviderHealthDisabled   ProviderHealthMode   = "disabled"
	ProviderHealthRequired   ProviderHealthMode   = "required"
	ReplicateWebhookDisabled ReplicateWebhookMode = "disabled"
	ReplicateWebhookRequired ReplicateWebhookMode = "required"
	FalWebhookDisabled       FalWebhookMode       = "disabled"
	FalWebhookRequired       FalWebhookMode       = "required"
	JobManagementDisabled    JobManagementMode    = "disabled"
	JobManagementRequired    JobManagementMode    = "required"
	PluginDisabled           PluginMode           = "disabled"
	PluginOptional           PluginMode           = "optional"
	PluginRequired           PluginMode           = "required"
	PluginRegistryDisabled   PluginRegistryMode   = "disabled"
	PluginRegistryRequired   PluginRegistryMode   = "required"
)

// Config contains non-provider process settings. Provider credentials remain
// in their opaque registry and are never exposed through this structure.
type Config struct {
	HTTPAddr                        string
	LogLevel                        slog.Level
	ShutdownTimeout                 time.Duration
	DatabaseURL                     string
	GoogleTimeout                   time.Duration
	GeminiStreamIdleTimeout         time.Duration
	GeminiBodyBytes                 int64
	GeminiLLMModels                 []string
	GeminiLLMModelLimits            map[string]ChatModelLimit
	ImagesTimeout                   time.Duration
	ImagesBodyBytes                 int64
	ChatTimeout                     time.Duration
	ChatStreamIdleTimeout           time.Duration
	ChatBodyBytes                   int64
	OpenAIChatModels                []string
	OpenAIChatModelLimits           map[string]ChatModelLimit
	OpenAIChatRoutes                []ChatRoute
	OpenAIResponsesModels           []string
	OpenAIResponsesModelLimits      map[string]ChatModelLimit
	OpenAIResponsesRoutes           []ResponsesRoute
	OpenAISpeechModels              []string
	SpeechTimeout                   time.Duration
	SpeechStreamIdleTimeout         time.Duration
	SpeechRequestBytes              int64
	SpeechResponseBytes             int64
	OpenAITranscriptionModels       []string
	OpenAITranscriptionCapabilities map[string]audiooperation.TranscriptionCapabilities
	TranscriptionTimeout            time.Duration
	TranscriptionStreamIdleTimeout  time.Duration
	TranscriptionRequestBytes       int64
	TranscriptionFileBytes          int64
	TranscriptionFieldBytes         int64
	TranscriptionResponseBytes      int64
	TranscriptionSpoolLimit         int
	OpenAITranslationModels         []string
	OpenAITranslationModelMap       map[string]string
	OpenAITranslationCapabilities   map[string]audiooperation.TranslationCapabilities
	TranslationTimeout              time.Duration
	TranslationRequestBytes         int64
	TranslationFileBytes            int64
	TranslationFieldBytes           int64
	TranslationResponseBytes        int64
	TranslationSpoolLimit           int
	ResponsesTimeout                time.Duration
	ResponsesStreamIdleTimeout      time.Duration
	ResponsesBodyBytes              int64
	AnthropicTimeout                time.Duration
	AnthropicStreamIdleTimeout      time.Duration
	AnthropicBodyBytes              int64
	AnthropicMessagesModels         []string
	AnthropicMessagesModelLimits    map[string]ChatModelLimit
	ImageEditsBodyBytes             int64
	ImageEditSpoolLimit             int
	BillingMode                     BillingMode
	MinimumMarginBPS                int64
	ReplayBodyBytes                 int64
	ReconcileInterval               time.Duration
	ReconcileLease                  time.Duration
	ReconcileBackoff                time.Duration
	ReconcileMaxBackoff             time.Duration
	ReconcileBatchSize              int
	ReconcileMaxAttempts            int
	RateLimitMode                   RateLimitMode
	RedisURL                        string
	RateLimitTimeout                time.Duration
	ProviderHealthMode              ProviderHealthMode
	ProviderHealth                  providerhealth.Config
	ImageStorage                    imagestorage.Config
	AudioInputStorage               audioassets.Config
	SpeechOutputStorage             speechstorage.Config
	VideoStorage                    videostorage.Config
	Telemetry                       telemetry.Config
	TrustedProxyPrefixes            []netip.Prefix
	ReplicateEnabled                bool
	ReplicateEndpoint               string
	ReplicateModels                 []string
	ReplicateTimeout                time.Duration
	ReplicateBodyBytes              int64
	ReplicateWebhookMode            ReplicateWebhookMode
	ReplicateWebhookSecrets         []string
	ReplicateWebhookCallbackSecret  []byte
	ReplicateWebhookTolerance       time.Duration
	ReplicateWebhookBindingTTL      time.Duration
	FalEnabled                      bool
	FalEndpoint                     string
	FalModels                       []string
	FalTimeout                      time.Duration
	FalBodyBytes                    int64
	FalWebhookMode                  FalWebhookMode
	FalWebhookCallbackSecret        []byte
	FalWebhookBindingTTL            time.Duration
	FalJWKSURL                      string
	FalJWKSTimeout                  time.Duration
	FalJWKSCacheTTL                 time.Duration
	FalJWKSRefreshCooldown          time.Duration
	RunwayEnabled                   bool
	RunwayModels                    []string
	RunwayModelCapabilities         map[string]videooperation.ModelCapability
	RunwayTimeout                   time.Duration
	RunwayBodyBytes                 int64
	RunwayPollInterval              time.Duration
	PublicBaseURL                   string
	JobManagementMode               JobManagementMode
	JobManagementCursorSecrets      [][]byte
	PluginMode                      PluginMode
	PluginManifestDir               string
	Plugins                         plugins.Config
	PluginRegistryMode              PluginRegistryMode
	PluginRegistryTrustFile         string
	PluginRegistryIndexFile         string
	PluginRegistryAdmissionDir      string
	PluginRegistryPlatform          string
	PluginRegistryMinimumSequence   uint64
}
type ChatModelLimit struct{ MaximumInputTokens, MaximumOutputTokens int64 }
type ChatRoute struct {
	Model               string          `json:"model"`
	Owner               string          `json:"owner"`
	Policy              string          `json:"policy"`
	FixedCandidateID    string          `json:"fixed_candidate_id"`
	MaximumInputTokens  int64           `json:"maximum_input_tokens"`
	MaximumOutputTokens int64           `json:"maximum_output_tokens"`
	Candidates          []ChatCandidate `json:"candidates"`
}
type ChatCandidate struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	ProviderModel string `json:"provider_model"`
	ChannelID     string `json:"channel_id"`
	Priority      int    `json:"priority"`
	Weight        uint32 `json:"weight"`
	Enabled       bool   `json:"enabled"`
	Streaming     bool   `json:"streaming"`
	Tools         bool   `json:"tools"`
	JSONMode      bool   `json:"json_mode"`
}
type ResponsesRoute struct {
	Model               string               `json:"model"`
	Owner               string               `json:"owner"`
	Policy              string               `json:"policy"`
	FixedCandidateID    string               `json:"fixed_candidate_id"`
	MaximumInputTokens  int64                `json:"maximum_input_tokens"`
	MaximumOutputTokens int64                `json:"maximum_output_tokens"`
	Candidates          []ResponsesCandidate `json:"candidates"`
}
type ResponsesCandidate struct {
	ID              string `json:"id"`
	Provider        string `json:"provider"`
	ProviderModel   string `json:"provider_model"`
	ChannelID       string `json:"channel_id"`
	Priority        int    `json:"priority"`
	Weight          uint32 `json:"weight"`
	Enabled         bool   `json:"enabled"`
	Streaming       bool   `json:"streaming"`
	FunctionTools   bool   `json:"function_tools"`
	WebSearch       bool   `json:"web_search"`
	XSearch         bool   `json:"x_search"`
	CodeInterpreter bool   `json:"code_interpreter"`
	ImageGeneration bool   `json:"image_generation"`
	JSONMode        bool   `json:"json_mode"`
	StoredResponse  bool   `json:"stored_response"`
}

// Load reads configuration through lookup and validates every value before
// the server starts. Errors name the setting but never echo its value.
func Load(lookup LookupEnv) (Config, error) {
	cfg := Config{
		HTTPAddr:                        defaultHTTPAddr,
		LogLevel:                        slog.LevelInfo,
		ShutdownTimeout:                 defaultShutdownTimeout,
		GoogleTimeout:                   defaultGoogleTimeout,
		GeminiStreamIdleTimeout:         30 * time.Second,
		GeminiBodyBytes:                 defaultGeminiBodyBytes,
		ImagesTimeout:                   defaultImagesTimeout,
		ImagesBodyBytes:                 defaultImagesBodyBytes,
		ChatTimeout:                     defaultImagesTimeout,
		ChatStreamIdleTimeout:           30 * time.Second,
		ChatBodyBytes:                   defaultChatBodyBytes,
		OpenAIChatModelLimits:           map[string]ChatModelLimit{},
		GeminiLLMModelLimits:            map[string]ChatModelLimit{},
		OpenAIResponsesModelLimits:      map[string]ChatModelLimit{},
		ResponsesTimeout:                defaultImagesTimeout,
		ResponsesStreamIdleTimeout:      30 * time.Second,
		ResponsesBodyBytes:              defaultChatBodyBytes,
		SpeechTimeout:                   defaultImagesTimeout,
		SpeechStreamIdleTimeout:         30 * time.Second,
		SpeechRequestBytes:              defaultSpeechRequestBytes,
		SpeechResponseBytes:             defaultSpeechResponseBytes,
		OpenAITranscriptionCapabilities: map[string]audiooperation.TranscriptionCapabilities{},
		TranscriptionTimeout:            defaultImagesTimeout,
		TranscriptionStreamIdleTimeout:  30 * time.Second,
		TranscriptionRequestBytes:       defaultTranscriptionRequestBytes,
		TranscriptionFileBytes:          defaultTranscriptionFileBytes,
		TranscriptionFieldBytes:         defaultTranscriptionFieldBytes,
		TranscriptionResponseBytes:      defaultTranscriptionResponseBytes,
		TranscriptionSpoolLimit:         8,
		OpenAITranslationModelMap:       map[string]string{},
		OpenAITranslationCapabilities:   map[string]audiooperation.TranslationCapabilities{},
		TranslationTimeout:              defaultImagesTimeout,
		TranslationRequestBytes:         defaultTranscriptionRequestBytes,
		TranslationFileBytes:            defaultTranscriptionFileBytes,
		TranslationFieldBytes:           defaultTranscriptionFieldBytes,
		TranslationResponseBytes:        defaultTranscriptionResponseBytes,
		TranslationSpoolLimit:           8,
		AnthropicTimeout:                defaultImagesTimeout,
		AnthropicStreamIdleTimeout:      30 * time.Second,
		AnthropicBodyBytes:              defaultChatBodyBytes,
		AnthropicMessagesModelLimits:    map[string]ChatModelLimit{},
		ImageEditsBodyBytes:             defaultImageEditsBodyBytes,
		ImageEditSpoolLimit:             8,
		BillingMode:                     BillingDisabled,
		ReplayBodyBytes:                 defaultReplayBodyBytes,
		ReconcileInterval:               defaultReconcileInterval,
		ReconcileLease:                  defaultReconcileLease,
		ReconcileBackoff:                defaultReconcileBackoff,
		ReconcileMaxBackoff:             defaultReconcileMaxBackoff,
		ReconcileBatchSize:              10,
		ReconcileMaxAttempts:            5,
		RateLimitMode:                   RateLimitDisabled,
		RateLimitTimeout:                defaultRateLimitTimeout,
		ProviderHealthMode:              ProviderHealthDisabled,
		ProviderHealth:                  providerhealth.DefaultConfig(),
		ImageStorage:                    imagestorage.DefaultConfig(),
		AudioInputStorage:               audioassets.DefaultConfig(),
		SpeechOutputStorage:             speechstorage.DefaultConfig(),
		VideoStorage:                    videostorage.DefaultConfig(),
		Telemetry:                       telemetry.DefaultConfig(),
		ReplicateEndpoint:               "https://api.replicate.com",
		ReplicateTimeout:                defaultReplicateTimeout,
		ReplicateBodyBytes:              defaultReplicateBodyBytes,
		ReplicateWebhookMode:            ReplicateWebhookDisabled,
		ReplicateWebhookTolerance:       defaultWebhookTolerance,
		ReplicateWebhookBindingTTL:      defaultWebhookBindingTTL,
		FalEndpoint:                     "https://queue.fal.run",
		FalTimeout:                      defaultFalTimeout,
		FalBodyBytes:                    defaultFalBodyBytes,
		FalWebhookMode:                  FalWebhookDisabled,
		FalWebhookBindingTTL:            defaultWebhookBindingTTL,
		FalJWKSURL:                      defaultFalJWKSURL,
		FalJWKSTimeout:                  defaultFalJWKSTimeout,
		FalJWKSCacheTTL:                 defaultFalJWKSCacheTTL,
		FalJWKSRefreshCooldown:          defaultFalJWKSRefresh,
		RunwayTimeout:                   defaultRunwayTimeout,
		RunwayBodyBytes:                 defaultRunwayBodyBytes,
		RunwayPollInterval:              5 * time.Second,
		JobManagementMode:               JobManagementDisabled,
		PluginMode:                      PluginDisabled,
		PluginRegistryMode:              PluginRegistryDisabled,
		PluginRegistryMinimumSequence:   1,
		Plugins:                         plugins.Config{Timeout: 2 * time.Minute, MaximumRequestBytes: 2 << 20, MaximumResponseBytes: 64 << 20, MaximumConcurrency: 16, EndpointOrigins: map[string]string{}, AuthSecrets: map[string]string{}, ResultOrigins: map[string][]string{}},
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
	if value, ok := lookup("GATEWAY_GEMINI_STREAM_IDLE_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return Config{}, fmt.Errorf("GATEWAY_GEMINI_STREAM_IDLE_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.GeminiStreamIdleTimeout = duration
	}
	if value, ok := lookup("GATEWAY_GEMINI_MAX_REQUEST_BODY_BYTES"); ok {
		bodyBytes, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || bodyBytes <= 0 || bodyBytes > defaultGeminiBodyBytes {
			return Config{}, fmt.Errorf("GATEWAY_GEMINI_MAX_REQUEST_BODY_BYTES: must be an integer between 1 and 33554432")
		}
		cfg.GeminiBodyBytes = bodyBytes
	}
	if value, ok := lookup("GATEWAY_GEMINI_LLM_MODELS"); ok {
		seen := map[string]bool{}
		for _, part := range strings.Split(value, ",") {
			model := strings.TrimSpace(part)
			if model == "" || len(model) > 200 || seen[model] || model == "gemini-image" {
				return Config{}, fmt.Errorf("GATEWAY_GEMINI_LLM_MODELS: must contain unique non-image Gemini model IDs")
			}
			for _, character := range model {
				if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-') {
					return Config{}, fmt.Errorf("GATEWAY_GEMINI_LLM_MODELS: contains an invalid model ID")
				}
			}
			seen[model] = true
			cfg.GeminiLLMModels = append(cfg.GeminiLLMModels, model)
		}
	}
	if value, ok := lookup("GATEWAY_GEMINI_LLM_MODEL_LIMITS"); ok {
		for _, part := range strings.Split(value, ",") {
			fields := strings.Split(strings.TrimSpace(part), ":")
			if len(fields) != 3 {
				return Config{}, fmt.Errorf("GATEWAY_GEMINI_LLM_MODEL_LIMITS: expected model:maximum_input:maximum_output")
			}
			input, inputErr := strconv.ParseInt(fields[1], 10, 64)
			output, outputErr := strconv.ParseInt(fields[2], 10, 64)
			if fields[0] == "" || inputErr != nil || outputErr != nil || input < 1 || output < 1 || input > 10_000_000 || output > 1_000_000 {
				return Config{}, fmt.Errorf("GATEWAY_GEMINI_LLM_MODEL_LIMITS: invalid model limit")
			}
			if _, exists := cfg.GeminiLLMModelLimits[fields[0]]; exists {
				return Config{}, fmt.Errorf("GATEWAY_GEMINI_LLM_MODEL_LIMITS: duplicate model")
			}
			cfg.GeminiLLMModelLimits[fields[0]] = ChatModelLimit{input, output}
		}
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
	if value, ok := lookup("GATEWAY_OPENAI_CHAT_REQUEST_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > maxImagesTimeout {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_CHAT_REQUEST_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.ChatTimeout = duration
	}
	if value, ok := lookup("GATEWAY_OPENAI_CHAT_STREAM_IDLE_TIMEOUT"); ok {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_CHAT_STREAM_IDLE_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.ChatStreamIdleTimeout = duration
	}
	if value, ok := lookup("GATEWAY_OPENAI_CHAT_MAX_BODY_BYTES"); ok {
		bodyBytes, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || bodyBytes <= 0 || bodyBytes > 32*1024*1024 {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_CHAT_MAX_BODY_BYTES: must be an integer between 1 and 33554432")
		}
		cfg.ChatBodyBytes = bodyBytes
	}
	if value, ok := lookup("GATEWAY_OPENAI_CHAT_MODELS"); ok {
		seen := map[string]bool{}
		for _, part := range strings.Split(value, ",") {
			model := strings.TrimSpace(part)
			if model == "" || len(model) > 200 || seen[model] {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_CHAT_MODELS: must contain unique non-empty model IDs")
			}
			for _, character := range model {
				if character <= 0x20 || character == 0x7f {
					return Config{}, fmt.Errorf("GATEWAY_OPENAI_CHAT_MODELS: contains an invalid model ID")
				}
			}
			seen[model] = true
			cfg.OpenAIChatModels = append(cfg.OpenAIChatModels, model)
		}
	}
	if value, ok := lookup("GATEWAY_OPENAI_CHAT_ROUTES_JSON"); ok {
		decoder := json.NewDecoder(bytes.NewBufferString(value))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg.OpenAIChatRoutes); err != nil || decoder.Decode(&struct{}{}) != io.EOF || len(cfg.OpenAIChatRoutes) == 0 || len(cfg.OpenAIChatRoutes) > 1000 {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_CHAT_ROUTES_JSON: must contain a bounded valid route array")
		}
		if len(cfg.OpenAIChatModels) > 0 {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_CHAT_ROUTES_JSON: cannot be combined with GATEWAY_OPENAI_CHAT_MODELS")
		}
		seen := map[string]bool{}
		for _, route := range cfg.OpenAIChatRoutes {
			if route.Model == "" || seen[route.Model] || len(route.Model) > 200 || len(route.Candidates) == 0 || len(route.Candidates) > 128 || route.MaximumInputTokens < 0 || route.MaximumOutputTokens < 0 || (route.MaximumInputTokens == 0) != (route.MaximumOutputTokens == 0) {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_CHAT_ROUTES_JSON: contains an invalid route")
			}
			seen[route.Model] = true
			cfg.OpenAIChatModels = append(cfg.OpenAIChatModels, route.Model)
			if route.MaximumInputTokens > 0 {
				cfg.OpenAIChatModelLimits[route.Model] = ChatModelLimit{route.MaximumInputTokens, route.MaximumOutputTokens}
			}
		}
	}
	if value, ok := lookup("GATEWAY_OPENAI_RESPONSES_MODELS"); ok {
		seen := map[string]bool{}
		for _, part := range strings.Split(value, ",") {
			model := strings.TrimSpace(part)
			if model == "" || len(model) > 200 || seen[model] {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_RESPONSES_MODELS: models must be unique non-empty values no longer than 200 bytes")
			}
			seen[model] = true
			cfg.OpenAIResponsesModels = append(cfg.OpenAIResponsesModels, model)
		}
	}
	if value, ok := lookup("GATEWAY_OPENAI_RESPONSES_ROUTES_JSON"); ok {
		decoder := json.NewDecoder(bytes.NewBufferString(value))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg.OpenAIResponsesRoutes); err != nil || decoder.Decode(&struct{}{}) != io.EOF || len(cfg.OpenAIResponsesRoutes) == 0 || len(cfg.OpenAIResponsesRoutes) > 1000 {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_RESPONSES_ROUTES_JSON: must contain a bounded valid route array")
		}
		if len(cfg.OpenAIResponsesModels) > 0 {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_RESPONSES_ROUTES_JSON: cannot be combined with GATEWAY_OPENAI_RESPONSES_MODELS")
		}
		seen := map[string]bool{}
		for _, route := range cfg.OpenAIResponsesRoutes {
			if route.Model == "" || seen[route.Model] || len(route.Model) > 200 || len(route.Candidates) == 0 || len(route.Candidates) > 128 || route.MaximumInputTokens < 0 || route.MaximumOutputTokens < 0 || (route.MaximumInputTokens == 0) != (route.MaximumOutputTokens == 0) {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_RESPONSES_ROUTES_JSON: contains an invalid route")
			}
			seen[route.Model] = true
			cfg.OpenAIResponsesModels = append(cfg.OpenAIResponsesModels, route.Model)
			if route.MaximumInputTokens > 0 {
				cfg.OpenAIResponsesModelLimits[route.Model] = ChatModelLimit{route.MaximumInputTokens, route.MaximumOutputTokens}
			}
		}
	}
	if value, ok := lookup("GATEWAY_OPENAI_RESPONSES_REQUEST_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_RESPONSES_REQUEST_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.ResponsesTimeout = duration
	}
	if value, ok := lookup("GATEWAY_OPENAI_RESPONSES_STREAM_IDLE_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_RESPONSES_STREAM_IDLE_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.ResponsesStreamIdleTimeout = duration
	}
	if value, ok := lookup("GATEWAY_OPENAI_RESPONSES_MAX_BODY_BYTES"); ok {
		bodyBytes, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || bodyBytes <= 0 || bodyBytes > 32*1024*1024 {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_RESPONSES_MAX_BODY_BYTES: must be an integer between 1 and 33554432")
		}
		cfg.ResponsesBodyBytes = bodyBytes
	}
	if value, ok := lookup("GATEWAY_OPENAI_SPEECH_MODELS"); ok {
		seen := map[string]bool{}
		for _, part := range strings.Split(value, ",") {
			model := strings.TrimSpace(part)
			if model == "" || len(model) > 200 || seen[model] || strings.ContainsAny(model, "\r\n") {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_SPEECH_MODELS: must contain unique valid model IDs")
			}
			seen[model] = true
			cfg.OpenAISpeechModels = append(cfg.OpenAISpeechModels, model)
		}
	}
	if value, ok := lookup("GATEWAY_OPENAI_SPEECH_REQUEST_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_SPEECH_REQUEST_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.SpeechTimeout = duration
	}
	if value, ok := lookup("GATEWAY_OPENAI_SPEECH_STREAM_IDLE_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_SPEECH_STREAM_IDLE_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.SpeechStreamIdleTimeout = duration
	}
	for key, target := range map[string]*int64{"GATEWAY_OPENAI_SPEECH_MAX_REQUEST_BODY_BYTES": &cfg.SpeechRequestBytes, "GATEWAY_OPENAI_SPEECH_MAX_RESPONSE_BODY_BYTES": &cfg.SpeechResponseBytes} {
		if value, ok := lookup(key); ok {
			limit, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			maximum := defaultSpeechRequestBytes
			if key == "GATEWAY_OPENAI_SPEECH_MAX_RESPONSE_BODY_BYTES" {
				maximum = 2 * 1024 * 1024 * 1024
			}
			if err != nil || limit < 1 || limit > maximum {
				return Config{}, fmt.Errorf("%s: must be a bounded positive integer", key)
			}
			*target = limit
		}
	}
	if value, ok := lookup("GATEWAY_OPENAI_TRANSCRIPTION_MODELS"); ok {
		seen := map[string]bool{}
		for _, part := range strings.Split(value, ",") {
			model := strings.TrimSpace(part)
			if model == "" || len(model) > 200 || seen[model] || strings.ContainsAny(model, "\r\n") {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_TRANSCRIPTION_MODELS: must contain unique valid model IDs")
			}
			seen[model] = true
			cfg.OpenAITranscriptionModels = append(cfg.OpenAITranscriptionModels, model)
		}
	}
	if value, ok := lookup("GATEWAY_OPENAI_TRANSCRIPTION_MODEL_CAPABILITIES_JSON"); ok {
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg.OpenAITranscriptionCapabilities); err != nil || decoder.Decode(&struct{}{}) != io.EOF || len(cfg.OpenAITranscriptionCapabilities) > 128 {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_TRANSCRIPTION_MODEL_CAPABILITIES_JSON: must be a bounded object")
		}
	}
	if value, ok := lookup("GATEWAY_OPENAI_TRANSCRIPTION_REQUEST_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_TRANSCRIPTION_REQUEST_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.TranscriptionTimeout = duration
	}
	if value, ok := lookup("GATEWAY_OPENAI_TRANSCRIPTION_STREAM_IDLE_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_TRANSCRIPTION_STREAM_IDLE_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.TranscriptionStreamIdleTimeout = duration
	}
	for key, target := range map[string]*int64{"GATEWAY_OPENAI_TRANSCRIPTION_MAX_REQUEST_BODY_BYTES": &cfg.TranscriptionRequestBytes, "GATEWAY_OPENAI_TRANSCRIPTION_MAX_FILE_BYTES": &cfg.TranscriptionFileBytes, "GATEWAY_OPENAI_TRANSCRIPTION_MAX_FIELD_BYTES": &cfg.TranscriptionFieldBytes, "GATEWAY_OPENAI_TRANSCRIPTION_MAX_RESPONSE_BODY_BYTES": &cfg.TranscriptionResponseBytes} {
		if value, ok := lookup(key); ok {
			limit, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil || limit < 1 || limit > 512*1024*1024 {
				return Config{}, fmt.Errorf("%s: must be a bounded positive integer", key)
			}
			*target = limit
		}
	}
	if value, ok := lookup("GATEWAY_OPENAI_TRANSCRIPTION_MAX_CONCURRENT_SPOOLS"); ok {
		limit, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || limit < 1 || limit > 128 {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_TRANSCRIPTION_MAX_CONCURRENT_SPOOLS: must be an integer between 1 and 128")
		}
		cfg.TranscriptionSpoolLimit = limit
	}
	if value, ok := lookup("GATEWAY_OPENAI_TRANSLATION_MODELS"); ok {
		seen := map[string]bool{}
		for _, part := range strings.Split(value, ",") {
			model := strings.TrimSpace(part)
			if model == "" || len(model) > 200 || seen[model] || strings.ContainsAny(model, "\r\n") {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_TRANSLATION_MODELS: must contain unique valid model IDs")
			}
			seen[model] = true
			cfg.OpenAITranslationModels = append(cfg.OpenAITranslationModels, model)
		}
	}
	for key, target := range map[string]any{"GATEWAY_OPENAI_TRANSLATION_MODEL_MAP": &cfg.OpenAITranslationModelMap, "GATEWAY_OPENAI_TRANSLATION_MODEL_CAPABILITIES_JSON": &cfg.OpenAITranslationCapabilities} {
		if value, ok := lookup(key); ok {
			decoder := json.NewDecoder(strings.NewReader(value))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(target); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
				return Config{}, fmt.Errorf("%s: must be a bounded object", key)
			}
		}
	}
	if len(cfg.OpenAITranslationModelMap) > 128 || len(cfg.OpenAITranslationCapabilities) > 128 {
		return Config{}, fmt.Errorf("GATEWAY_OPENAI_TRANSLATION_MODEL_CAPABILITIES_JSON: must be a bounded object")
	}
	if value, ok := lookup("GATEWAY_OPENAI_TRANSLATION_REQUEST_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_TRANSLATION_REQUEST_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.TranslationTimeout = duration
	}
	for key, target := range map[string]*int64{"GATEWAY_OPENAI_TRANSLATION_MAX_REQUEST_BODY_BYTES": &cfg.TranslationRequestBytes, "GATEWAY_OPENAI_TRANSLATION_MAX_FILE_BYTES": &cfg.TranslationFileBytes, "GATEWAY_OPENAI_TRANSLATION_MAX_FIELD_BYTES": &cfg.TranslationFieldBytes, "GATEWAY_OPENAI_TRANSLATION_MAX_RESPONSE_BODY_BYTES": &cfg.TranslationResponseBytes} {
		if value, ok := lookup(key); ok {
			limit, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil || limit < 1 || limit > 512*1024*1024 {
				return Config{}, fmt.Errorf("%s: must be a bounded positive integer", key)
			}
			*target = limit
		}
	}
	if value, ok := lookup("GATEWAY_OPENAI_TRANSLATION_MAX_CONCURRENT_SPOOLS"); ok {
		limit, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || limit < 1 || limit > 128 {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_TRANSLATION_MAX_CONCURRENT_SPOOLS: must be an integer between 1 and 128")
		}
		cfg.TranslationSpoolLimit = limit
	}
	if value, ok := lookup("GATEWAY_ANTHROPIC_REQUEST_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return Config{}, fmt.Errorf("GATEWAY_ANTHROPIC_REQUEST_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.AnthropicTimeout = duration
	}
	if value, ok := lookup("GATEWAY_ANTHROPIC_STREAM_IDLE_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return Config{}, fmt.Errorf("GATEWAY_ANTHROPIC_STREAM_IDLE_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.AnthropicStreamIdleTimeout = duration
	}
	if value, ok := lookup("GATEWAY_ANTHROPIC_MAX_BODY_BYTES"); ok {
		bodyBytes, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || bodyBytes <= 0 || bodyBytes > 32*1024*1024 {
			return Config{}, fmt.Errorf("GATEWAY_ANTHROPIC_MAX_BODY_BYTES: must be an integer between 1 and 33554432")
		}
		cfg.AnthropicBodyBytes = bodyBytes
	}
	if value, ok := lookup("GATEWAY_ANTHROPIC_MESSAGES_MODELS"); ok {
		seen := map[string]bool{}
		for _, part := range strings.Split(value, ",") {
			model := strings.TrimSpace(part)
			if model == "" || len(model) > 200 || seen[model] {
				return Config{}, fmt.Errorf("GATEWAY_ANTHROPIC_MESSAGES_MODELS: must contain unique non-empty model IDs")
			}
			for _, character := range model {
				if character <= 0x20 || character == 0x7f {
					return Config{}, fmt.Errorf("GATEWAY_ANTHROPIC_MESSAGES_MODELS: contains an invalid model ID")
				}
			}
			seen[model] = true
			cfg.AnthropicMessagesModels = append(cfg.AnthropicMessagesModels, model)
		}
	}
	if value, ok := lookup("GATEWAY_ANTHROPIC_MESSAGES_MODEL_LIMITS"); ok {
		for _, part := range strings.Split(value, ",") {
			fields := strings.Split(strings.TrimSpace(part), ":")
			if len(fields) != 3 {
				return Config{}, fmt.Errorf("GATEWAY_ANTHROPIC_MESSAGES_MODEL_LIMITS: expected model:maximum_input:maximum_output")
			}
			input, inputErr := strconv.ParseInt(fields[1], 10, 64)
			output, outputErr := strconv.ParseInt(fields[2], 10, 64)
			if fields[0] == "" || inputErr != nil || outputErr != nil || input < 1 || output < 1 || input > 10_000_000 || output > 1_000_000 {
				return Config{}, fmt.Errorf("GATEWAY_ANTHROPIC_MESSAGES_MODEL_LIMITS: invalid model limit")
			}
			if _, exists := cfg.AnthropicMessagesModelLimits[fields[0]]; exists {
				return Config{}, fmt.Errorf("GATEWAY_ANTHROPIC_MESSAGES_MODEL_LIMITS: duplicate model")
			}
			cfg.AnthropicMessagesModelLimits[fields[0]] = ChatModelLimit{input, output}
		}
	}
	if value, ok := lookup("GATEWAY_OPENAI_RESPONSES_MODEL_LIMITS"); ok {
		for _, part := range strings.Split(value, ",") {
			fields := strings.Split(strings.TrimSpace(part), ":")
			if len(fields) != 3 {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_RESPONSES_MODEL_LIMITS: expected model:maximum_input:maximum_output")
			}
			input, inputErr := strconv.ParseInt(fields[1], 10, 64)
			output, outputErr := strconv.ParseInt(fields[2], 10, 64)
			if fields[0] == "" || inputErr != nil || outputErr != nil || input < 1 || output < 1 || input > 10_000_000 || output > 1_000_000 {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_RESPONSES_MODEL_LIMITS: invalid model limit")
			}
			if _, exists := cfg.OpenAIResponsesModelLimits[fields[0]]; exists {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_RESPONSES_MODEL_LIMITS: duplicate model")
			}
			cfg.OpenAIResponsesModelLimits[fields[0]] = ChatModelLimit{input, output}
		}
	}
	if value, ok := lookup("GATEWAY_OPENAI_CHAT_MODEL_LIMITS"); ok {
		for _, part := range strings.Split(value, ",") {
			fields := strings.Split(strings.TrimSpace(part), ":")
			if len(fields) != 3 {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_CHAT_MODEL_LIMITS: expected model:maximum_input:maximum_output")
			}
			input, inputErr := strconv.ParseInt(fields[1], 10, 64)
			output, outputErr := strconv.ParseInt(fields[2], 10, 64)
			if fields[0] == "" || inputErr != nil || outputErr != nil || input < 1 || output < 1 || input > 10_000_000 || output > 1_000_000 {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_CHAT_MODEL_LIMITS: invalid model limit")
			}
			if _, exists := cfg.OpenAIChatModelLimits[fields[0]]; exists {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_CHAT_MODEL_LIMITS: duplicate model")
			}
			cfg.OpenAIChatModelLimits[fields[0]] = ChatModelLimit{input, output}
		}
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
	if err := loadAudioInputStorage(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := loadSpeechOutputStorage(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := loadVideoStorage(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := loadTelemetry(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := loadReplicate(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := loadFal(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := loadRunway(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := loadPlugins(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if value, ok := lookup("GATEWAY_JOB_MANAGEMENT_MODE"); ok {
		cfg.JobManagementMode = JobManagementMode(strings.TrimSpace(value))
	}
	if cfg.JobManagementMode != JobManagementDisabled && cfg.JobManagementMode != JobManagementRequired {
		return Config{}, fmt.Errorf("GATEWAY_JOB_MANAGEMENT_MODE: must be disabled or required")
	}
	if value, ok := lookup("GATEWAY_JOB_MANAGEMENT_CURSOR_SECRETS"); ok {
		parts := strings.Split(value, ",")
		if len(parts) < 1 || len(parts) > 2 {
			return Config{}, fmt.Errorf("GATEWAY_JOB_MANAGEMENT_CURSOR_SECRETS: must contain one or two base64 secrets")
		}
		for _, part := range parts {
			secret, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(part))
			if decodeErr != nil || len(secret) != 32 {
				return Config{}, fmt.Errorf("GATEWAY_JOB_MANAGEMENT_CURSOR_SECRETS: each secret must decode to exactly 32 bytes")
			}
			cfg.JobManagementCursorSecrets = append(cfg.JobManagementCursorSecrets, secret)
		}
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
	if cfg.ImageStorage.Mode == imagestorage.Managed && cfg.BillingMode == BillingRequired && cfg.ReplayBodyBytes < cfg.ImageStorage.MaximumTotalBytes*4/3+1<<20 {
		return Config{}, fmt.Errorf("GATEWAY_IDEMPOTENCY_MAX_RESPONSE_BYTES: must cover the managed image response limit")
	}
	if cfg.JobManagementMode == JobManagementRequired && len(cfg.JobManagementCursorSecrets) == 0 {
		return Config{}, fmt.Errorf("GATEWAY_JOB_MANAGEMENT_CURSOR_SECRETS: required when Job management is required")
	}
	if cfg.JobManagementMode == JobManagementRequired && !cfg.ReplicateEnabled && !cfg.FalEnabled && !cfg.RunwayEnabled {
		return Config{}, fmt.Errorf("GATEWAY_JOB_MANAGEMENT_MODE: requires an asynchronous provider")
	}
	if cfg.BillingMode == BillingRequired && len(cfg.OpenAIChatModels) > 0 {
		if len(cfg.OpenAIChatModelLimits) != len(cfg.OpenAIChatModels) {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_CHAT_MODEL_LIMITS: every paid Chat model requires limits")
		}
		for _, model := range cfg.OpenAIChatModels {
			if _, ok := cfg.OpenAIChatModelLimits[model]; !ok {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_CHAT_MODEL_LIMITS: every paid Chat model requires limits")
			}
		}
	}
	if cfg.BillingMode == BillingRequired && len(cfg.OpenAIResponsesModels) > 0 {
		if len(cfg.OpenAIResponsesModelLimits) != len(cfg.OpenAIResponsesModels) {
			return Config{}, fmt.Errorf("GATEWAY_OPENAI_RESPONSES_MODEL_LIMITS: every paid Responses model requires limits")
		}
		for _, model := range cfg.OpenAIResponsesModels {
			if _, ok := cfg.OpenAIResponsesModelLimits[model]; !ok {
				return Config{}, fmt.Errorf("GATEWAY_OPENAI_RESPONSES_MODEL_LIMITS: every paid Responses model requires limits")
			}
		}
	}
	if cfg.TranscriptionFileBytes > cfg.TranscriptionRequestBytes {
		return Config{}, fmt.Errorf("GATEWAY_OPENAI_TRANSCRIPTION_MAX_FILE_BYTES: cannot exceed request body limit")
	}
	if _, err := audiooperation.NewTranscriptionRegistry(cfg.OpenAITranscriptionModels, cfg.OpenAITranscriptionCapabilities); err != nil {
		return Config{}, fmt.Errorf("GATEWAY_OPENAI_TRANSCRIPTION_MODEL_CAPABILITIES_JSON: invalid model capabilities")
	}
	if cfg.TranslationFileBytes > cfg.TranslationRequestBytes {
		return Config{}, fmt.Errorf("GATEWAY_OPENAI_TRANSLATION_MAX_FILE_BYTES: cannot exceed request body limit")
	}
	if _, err := audiooperation.NewTranslationRegistry(cfg.OpenAITranslationModels, cfg.OpenAITranslationModelMap, cfg.OpenAITranslationCapabilities); err != nil {
		return Config{}, fmt.Errorf("GATEWAY_OPENAI_TRANSLATION_MODEL_CAPABILITIES_JSON: invalid model capabilities")
	}
	if cfg.BillingMode == BillingRequired && len(cfg.GeminiLLMModels) > 0 {
		if len(cfg.GeminiLLMModelLimits) != len(cfg.GeminiLLMModels) {
			return Config{}, fmt.Errorf("GATEWAY_GEMINI_LLM_MODEL_LIMITS: every paid Gemini LLM model requires limits")
		}
		for _, model := range cfg.GeminiLLMModels {
			if _, ok := cfg.GeminiLLMModelLimits[model]; !ok {
				return Config{}, fmt.Errorf("GATEWAY_GEMINI_LLM_MODEL_LIMITS: every paid Gemini LLM model requires limits")
			}
		}
	}
	if cfg.BillingMode == BillingRequired && len(cfg.AnthropicMessagesModels) > 0 {
		if len(cfg.AnthropicMessagesModelLimits) != len(cfg.AnthropicMessagesModels) {
			return Config{}, fmt.Errorf("GATEWAY_ANTHROPIC_MESSAGES_MODEL_LIMITS: every paid Anthropic model requires limits")
		}
		for _, model := range cfg.AnthropicMessagesModels {
			if _, ok := cfg.AnthropicMessagesModelLimits[model]; !ok {
				return Config{}, fmt.Errorf("GATEWAY_ANTHROPIC_MESSAGES_MODEL_LIMITS: every paid Anthropic model requires limits")
			}
		}
	}

	return cfg, nil
}

func loadRunway(cfg *Config, lookup LookupEnv) error {
	_, cfg.RunwayEnabled = lookup("GATEWAY_RUNWAY_API_KEY")
	if value, ok := lookup("GATEWAY_RUNWAY_MODELS"); ok {
		for _, part := range strings.Split(value, ",") {
			model := strings.TrimSpace(part)
			if model == "" || len(model) > 200 || strings.ContainsAny(model, "\r\n") {
				return fmt.Errorf("GATEWAY_RUNWAY_MODELS: invalid model")
			}
			cfg.RunwayModels = append(cfg.RunwayModels, model)
		}
	}
	if value, ok := lookup("GATEWAY_RUNWAY_MODEL_CAPABILITIES_JSON"); ok {
		if json.Unmarshal([]byte(value), &cfg.RunwayModelCapabilities) != nil || len(cfg.RunwayModelCapabilities) > 64 {
			return fmt.Errorf("GATEWAY_RUNWAY_MODEL_CAPABILITIES_JSON: invalid capability map")
		}
	}
	if value, ok := lookup("GATEWAY_RUNWAY_REQUEST_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return fmt.Errorf("GATEWAY_RUNWAY_REQUEST_TIMEOUT: invalid duration")
		}
		cfg.RunwayTimeout = duration
	}
	if value, ok := lookup("GATEWAY_RUNWAY_MAX_BODY_BYTES"); ok {
		limit, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || limit < 1024 || limit > 256*1024*1024 {
			return fmt.Errorf("GATEWAY_RUNWAY_MAX_BODY_BYTES: invalid limit")
		}
		cfg.RunwayBodyBytes = limit
	}
	if value, ok := lookup("GATEWAY_RUNWAY_POLL_INTERVAL"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration < 5*time.Second || duration > 10*time.Minute {
			return fmt.Errorf("GATEWAY_RUNWAY_POLL_INTERVAL: must be between 5s and 10m")
		}
		cfg.RunwayPollInterval = duration
	}
	if cfg.RunwayEnabled && len(cfg.RunwayModels) == 0 {
		return fmt.Errorf("GATEWAY_RUNWAY_MODELS: must not be empty when Runway is enabled")
	}
	if _, err := videooperation.NewRegistryWithCapabilities(cfg.RunwayModels, cfg.RunwayModelCapabilities); err != nil {
		return fmt.Errorf("GATEWAY_RUNWAY_MODEL_CAPABILITIES_JSON: invalid model capability")
	}
	return nil
}

func loadReplicate(cfg *Config, lookup LookupEnv) error {
	_, cfg.ReplicateEnabled = lookup("GATEWAY_REPLICATE_API_TOKEN")
	if value, ok := lookup("GATEWAY_REPLICATE_API_ENDPOINT"); ok {
		cfg.ReplicateEndpoint = strings.TrimSpace(value)
	}
	if value, ok := lookup("GATEWAY_PUBLIC_BASE_URL"); ok {
		cfg.PublicBaseURL = strings.TrimSuffix(strings.TrimSpace(value), "/")
	}
	if value, ok := lookup("GATEWAY_REPLICATE_MODELS"); ok {
		seen := map[string]struct{}{}
		for _, part := range strings.Split(value, ",") {
			model := strings.TrimSpace(part)
			if model == "" || len(model) > 200 {
				return fmt.Errorf("GATEWAY_REPLICATE_MODELS: must contain bounded comma-separated exact versions")
			}
			if _, exists := seen[model]; exists {
				continue
			}
			seen[model] = struct{}{}
			cfg.ReplicateModels = append(cfg.ReplicateModels, model)
		}
		if len(cfg.ReplicateModels) > 64 {
			return fmt.Errorf("GATEWAY_REPLICATE_MODELS: must contain no more than 64 versions")
		}
	}
	if value, ok := lookup("GATEWAY_REPLICATE_REQUEST_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return fmt.Errorf("GATEWAY_REPLICATE_REQUEST_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.ReplicateTimeout = duration
	}
	if value, ok := lookup("GATEWAY_REPLICATE_MAX_BODY_BYTES"); ok {
		limit, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || limit < 1 || limit > 256*1024*1024 {
			return fmt.Errorf("GATEWAY_REPLICATE_MAX_BODY_BYTES: must be between 1 and 268435456")
		}
		cfg.ReplicateBodyBytes = limit
	}
	if value, ok := lookup("GATEWAY_REPLICATE_WEBHOOK_MODE"); ok {
		switch ReplicateWebhookMode(strings.ToLower(strings.TrimSpace(value))) {
		case ReplicateWebhookDisabled:
			cfg.ReplicateWebhookMode = ReplicateWebhookDisabled
		case ReplicateWebhookRequired:
			cfg.ReplicateWebhookMode = ReplicateWebhookRequired
		default:
			return fmt.Errorf("GATEWAY_REPLICATE_WEBHOOK_MODE: must be disabled or required")
		}
	}
	if value, ok := lookup("GATEWAY_REPLICATE_WEBHOOK_SIGNING_SECRETS"); ok {
		for _, part := range strings.Split(value, ",") {
			secret := strings.TrimSpace(part)
			if !strings.HasPrefix(secret, "whsec_") || len(secret) > 512 {
				return fmt.Errorf("GATEWAY_REPLICATE_WEBHOOK_SIGNING_SECRETS: contains an invalid secret")
			}
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
			if err != nil || len(decoded) < 16 || len(decoded) > 128 {
				return fmt.Errorf("GATEWAY_REPLICATE_WEBHOOK_SIGNING_SECRETS: contains an invalid secret")
			}
			cfg.ReplicateWebhookSecrets = append(cfg.ReplicateWebhookSecrets, secret)
		}
		if len(cfg.ReplicateWebhookSecrets) > 2 {
			return fmt.Errorf("GATEWAY_REPLICATE_WEBHOOK_SIGNING_SECRETS: must contain at most two secrets")
		}
	}
	if value, ok := lookup("GATEWAY_REPLICATE_WEBHOOK_CALLBACK_SECRET"); ok {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
		if err != nil || len(decoded) != 32 {
			return fmt.Errorf("GATEWAY_REPLICATE_WEBHOOK_CALLBACK_SECRET: must be a base64-encoded 32-byte secret")
		}
		cfg.ReplicateWebhookCallbackSecret = decoded
	}
	if value, ok := lookup("GATEWAY_REPLICATE_WEBHOOK_TOLERANCE"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration < time.Minute || duration > 15*time.Minute {
			return fmt.Errorf("GATEWAY_REPLICATE_WEBHOOK_TOLERANCE: must be between 1m and 15m")
		}
		cfg.ReplicateWebhookTolerance = duration
	}
	if value, ok := lookup("GATEWAY_REPLICATE_WEBHOOK_BINDING_TTL"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration < time.Hour || duration > 30*24*time.Hour {
			return fmt.Errorf("GATEWAY_REPLICATE_WEBHOOK_BINDING_TTL: must be between 1h and 720h")
		}
		cfg.ReplicateWebhookBindingTTL = duration
	}
	for key, value := range map[string]string{"GATEWAY_REPLICATE_API_ENDPOINT": cfg.ReplicateEndpoint, "GATEWAY_PUBLIC_BASE_URL": cfg.PublicBaseURL} {
		if value == "" {
			if cfg.ReplicateEnabled {
				return fmt.Errorf("%s: must not be empty when Replicate is enabled", key)
			}
			continue
		}
		parsed, err := url.Parse(value)
		loopback := err == nil && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1")
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback)) {
			return fmt.Errorf("%s: must be an HTTPS or loopback HTTP origin", key)
		}
	}
	if cfg.ReplicateEnabled && len(cfg.ReplicateModels) == 0 {
		return fmt.Errorf("GATEWAY_REPLICATE_MODELS: must not be empty when Replicate is enabled")
	}
	if cfg.ReplicateWebhookMode == ReplicateWebhookRequired {
		parsed, _ := url.Parse(cfg.PublicBaseURL)
		if !cfg.ReplicateEnabled {
			return fmt.Errorf("GATEWAY_REPLICATE_WEBHOOK_MODE: required needs Replicate enabled")
		}
		if parsed == nil || parsed.Scheme != "https" {
			return fmt.Errorf("GATEWAY_PUBLIC_BASE_URL: must be HTTPS when Replicate webhooks are required")
		}
		if len(cfg.ReplicateWebhookSecrets) == 0 {
			return fmt.Errorf("GATEWAY_REPLICATE_WEBHOOK_SIGNING_SECRETS: required when Replicate webhooks are required")
		}
		if len(cfg.ReplicateWebhookCallbackSecret) != 32 {
			return fmt.Errorf("GATEWAY_REPLICATE_WEBHOOK_CALLBACK_SECRET: required when Replicate webhooks are required")
		}
	}
	return nil
}

func loadFal(cfg *Config, lookup LookupEnv) error {
	_, cfg.FalEnabled = lookup("GATEWAY_FAL_API_KEY")
	if value, ok := lookup("GATEWAY_FAL_QUEUE_ENDPOINT"); ok {
		cfg.FalEndpoint = strings.TrimSpace(value)
	}
	if value, ok := lookup("GATEWAY_FAL_MODELS"); ok {
		seen := map[string]struct{}{}
		for _, part := range strings.Split(value, ",") {
			model := strings.TrimSpace(part)
			segments := strings.Split(model, "/")
			if model == "" || len(model) > 200 || len(segments) < 2 {
				return fmt.Errorf("GATEWAY_FAL_MODELS: must contain bounded comma-separated model IDs")
			}
			for _, segment := range segments {
				if segment == "" || segment == "." || segment == ".." {
					return fmt.Errorf("GATEWAY_FAL_MODELS: contains an invalid model ID")
				}
			}
			if _, exists := seen[model]; exists {
				continue
			}
			seen[model] = struct{}{}
			cfg.FalModels = append(cfg.FalModels, model)
		}
		if len(cfg.FalModels) > 64 {
			return fmt.Errorf("GATEWAY_FAL_MODELS: must contain no more than 64 models")
		}
	}
	if value, ok := lookup("GATEWAY_FAL_REQUEST_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 10*time.Minute {
			return fmt.Errorf("GATEWAY_FAL_REQUEST_TIMEOUT: must be a positive duration no greater than 10m")
		}
		cfg.FalTimeout = duration
	}
	if value, ok := lookup("GATEWAY_FAL_MAX_BODY_BYTES"); ok {
		limit, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || limit < 1 || limit > 256*1024*1024 {
			return fmt.Errorf("GATEWAY_FAL_MAX_BODY_BYTES: must be between 1 and 268435456")
		}
		cfg.FalBodyBytes = limit
	}
	if value, ok := lookup("GATEWAY_FAL_WEBHOOK_MODE"); ok {
		switch FalWebhookMode(strings.ToLower(strings.TrimSpace(value))) {
		case FalWebhookDisabled:
			cfg.FalWebhookMode = FalWebhookDisabled
		case FalWebhookRequired:
			cfg.FalWebhookMode = FalWebhookRequired
		default:
			return fmt.Errorf("GATEWAY_FAL_WEBHOOK_MODE: must be disabled or required")
		}
	}
	if value, ok := lookup("GATEWAY_FAL_WEBHOOK_CALLBACK_SECRET"); ok {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
		if err != nil || len(decoded) != 32 {
			return fmt.Errorf("GATEWAY_FAL_WEBHOOK_CALLBACK_SECRET: must be a base64-encoded 32-byte secret")
		}
		cfg.FalWebhookCallbackSecret = decoded
	}
	if value, ok := lookup("GATEWAY_FAL_WEBHOOK_BINDING_TTL"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration < time.Hour || duration > 30*24*time.Hour {
			return fmt.Errorf("GATEWAY_FAL_WEBHOOK_BINDING_TTL: must be between 1h and 720h")
		}
		cfg.FalWebhookBindingTTL = duration
	}
	if value, ok := lookup("GATEWAY_FAL_JWKS_URL"); ok {
		cfg.FalJWKSURL = strings.TrimSpace(value)
	}
	for _, setting := range []struct {
		key    string
		target *time.Duration
		min    time.Duration
		max    time.Duration
	}{
		{"GATEWAY_FAL_JWKS_TIMEOUT", &cfg.FalJWKSTimeout, time.Millisecond, time.Minute},
		{"GATEWAY_FAL_JWKS_CACHE_TTL", &cfg.FalJWKSCacheTTL, time.Minute, 24 * time.Hour},
		{"GATEWAY_FAL_JWKS_REFRESH_COOLDOWN", &cfg.FalJWKSRefreshCooldown, time.Second, time.Hour},
	} {
		if value, ok := lookup(setting.key); ok {
			duration, err := time.ParseDuration(strings.TrimSpace(value))
			if err != nil || duration < setting.min || duration > setting.max {
				return fmt.Errorf("%s: duration is outside the allowed range", setting.key)
			}
			*setting.target = duration
		}
	}
	for key, value := range map[string]string{"GATEWAY_FAL_QUEUE_ENDPOINT": cfg.FalEndpoint, "GATEWAY_PUBLIC_BASE_URL": cfg.PublicBaseURL} {
		if value == "" {
			if cfg.FalEnabled {
				return fmt.Errorf("%s: must not be empty when fal is enabled", key)
			}
			continue
		}
		parsed, err := url.Parse(value)
		loopback := err == nil && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1")
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback)) {
			return fmt.Errorf("%s: must be an HTTPS or loopback HTTP origin", key)
		}
	}
	if cfg.FalEnabled && len(cfg.FalModels) == 0 {
		return fmt.Errorf("GATEWAY_FAL_MODELS: must not be empty when fal is enabled")
	}
	if cfg.FalWebhookMode == FalWebhookRequired {
		public, _ := url.Parse(cfg.PublicBaseURL)
		jwks, err := url.Parse(cfg.FalJWKSURL)
		loopback := err == nil && (jwks.Hostname() == "localhost" || jwks.Hostname() == "127.0.0.1" || jwks.Hostname() == "::1")
		if !cfg.FalEnabled {
			return fmt.Errorf("GATEWAY_FAL_WEBHOOK_MODE: required needs fal enabled")
		}
		if public == nil || public.Scheme != "https" {
			return fmt.Errorf("GATEWAY_PUBLIC_BASE_URL: must be HTTPS when fal webhooks are required")
		}
		if len(cfg.FalWebhookCallbackSecret) != 32 {
			return fmt.Errorf("GATEWAY_FAL_WEBHOOK_CALLBACK_SECRET: required when fal webhooks are required")
		}
		if err != nil || jwks.Host == "" || jwks.User != nil || jwks.RawQuery != "" || jwks.Fragment != "" || jwks.Path != "/.well-known/jwks.json" || (jwks.Scheme != "https" && !(jwks.Scheme == "http" && loopback)) {
			return fmt.Errorf("GATEWAY_FAL_JWKS_URL: must be the HTTPS fal JWKS endpoint")
		}
	}
	return nil
}

func loadTelemetry(cfg *Config, lookup LookupEnv) error {
	if value, ok := lookup("GATEWAY_TELEMETRY_MODE"); ok {
		cfg.Telemetry.Mode = telemetry.Mode(strings.ToLower(strings.TrimSpace(value)))
	}
	for _, setting := range []struct {
		key    string
		target *string
	}{
		{"GATEWAY_TELEMETRY_OTLP_ENDPOINT", &cfg.Telemetry.Endpoint},
		{"GATEWAY_TELEMETRY_OTLP_AUTHORIZATION", &cfg.Telemetry.Authorization},
		{"GATEWAY_TELEMETRY_SERVICE_NAME", &cfg.Telemetry.ServiceName},
		{"GATEWAY_TELEMETRY_SERVICE_VERSION", &cfg.Telemetry.ServiceVersion},
		{"GATEWAY_TELEMETRY_ENVIRONMENT", &cfg.Telemetry.Environment},
	} {
		if value, ok := lookup(setting.key); ok {
			*setting.target = strings.TrimSpace(value)
		}
	}
	if value, ok := lookup("GATEWAY_TELEMETRY_SAMPLE_RATIO"); ok {
		ratio, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return fmt.Errorf("GATEWAY_TELEMETRY_SAMPLE_RATIO: must be a number between 0 and 1")
		}
		cfg.Telemetry.SampleRatio = ratio
	}
	for _, setting := range []struct {
		key    string
		target *time.Duration
	}{
		{"GATEWAY_TELEMETRY_EXPORT_INTERVAL", &cfg.Telemetry.ExportInterval},
		{"GATEWAY_TELEMETRY_EXPORT_TIMEOUT", &cfg.Telemetry.ExportTimeout},
		{"GATEWAY_TELEMETRY_SHUTDOWN_TIMEOUT", &cfg.Telemetry.ShutdownTimeout},
	} {
		if value, ok := lookup(setting.key); ok {
			duration, err := time.ParseDuration(strings.TrimSpace(value))
			if err != nil {
				return fmt.Errorf("%s: must be a valid bounded duration", setting.key)
			}
			*setting.target = duration
		}
	}
	if err := cfg.Telemetry.Validate(); err != nil {
		return fmt.Errorf("GATEWAY_TELEMETRY_*: settings are invalid")
	}
	return nil
}

func loadImageStorage(cfg *Config, lookup LookupEnv) error {
	cfg.ImageStorage.FetchOrigins = map[string][]string{}
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
	for _, provider := range []string{"openai", "xai", "google"} {
		key := "GATEWAY_IMAGE_STORAGE_FETCH_ORIGINS_" + strings.ToUpper(provider)
		if value, ok := lookup(key); ok {
			parts := strings.Split(value, ",")
			origins := make([]string, 0, len(parts))
			for _, part := range parts {
				origins = append(origins, strings.TrimSpace(part))
			}
			cfg.ImageStorage.FetchOrigins[provider] = origins
		}
	}
	if err := cfg.ImageStorage.Validate(); err != nil {
		return fmt.Errorf("GATEWAY_IMAGE_STORAGE_*: settings are invalid")
	}
	return nil
}

func loadAudioInputStorage(cfg *Config, lookup LookupEnv) error {
	if value, ok := lookup("GATEWAY_AUDIO_INPUT_STORAGE_MODE"); ok {
		cfg.AudioInputStorage.Mode = audioassets.Mode(strings.ToLower(strings.TrimSpace(value)))
	}
	for _, setting := range []struct {
		key    string
		target *string
	}{{"GATEWAY_AUDIO_INPUT_STORAGE_ENDPOINT", &cfg.AudioInputStorage.Endpoint}, {"GATEWAY_AUDIO_INPUT_STORAGE_REGION", &cfg.AudioInputStorage.Region}, {"GATEWAY_AUDIO_INPUT_STORAGE_BUCKET", &cfg.AudioInputStorage.Bucket}, {"GATEWAY_AUDIO_INPUT_STORAGE_ACCESS_KEY_ID", &cfg.AudioInputStorage.AccessKeyID}, {"GATEWAY_AUDIO_INPUT_STORAGE_SECRET_ACCESS_KEY", &cfg.AudioInputStorage.SecretAccessKey}, {"GATEWAY_AUDIO_INPUT_STORAGE_SERVER_SIDE_ENCRYPTION", &cfg.AudioInputStorage.ServerSideEncryption}, {"GATEWAY_AUDIO_INPUT_STORAGE_TEMP_DIR", &cfg.AudioInputStorage.TemporaryDirectory}} {
		if value, ok := lookup(setting.key); ok {
			*setting.target = strings.TrimSpace(value)
		}
	}
	if value, ok := lookup("GATEWAY_AUDIO_INPUT_STORAGE_MAX_BYTES"); ok {
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return fmt.Errorf("GATEWAY_AUDIO_INPUT_STORAGE_MAX_BYTES: must be a valid bounded integer")
		}
		cfg.AudioInputStorage.MaximumBytes = parsed
	}
	if value, ok := lookup("GATEWAY_AUDIO_INPUT_STORAGE_MAX_CONCURRENT_UPLOADS"); ok {
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("GATEWAY_AUDIO_INPUT_STORAGE_MAX_CONCURRENT_UPLOADS: must be a valid bounded integer")
		}
		cfg.AudioInputStorage.MaximumConcurrentUploads = parsed
	}
	for _, setting := range []struct {
		key    string
		target *time.Duration
	}{{"GATEWAY_AUDIO_INPUT_STORAGE_UPLOAD_TIMEOUT", &cfg.AudioInputStorage.UploadTimeout}, {"GATEWAY_AUDIO_INPUT_STORAGE_DOWNLOAD_TIMEOUT", &cfg.AudioInputStorage.DownloadTimeout}, {"GATEWAY_AUDIO_INPUT_STORAGE_RETENTION", &cfg.AudioInputStorage.Retention}, {"GATEWAY_AUDIO_INPUT_STORAGE_CLEANUP_INTERVAL", &cfg.AudioInputStorage.CleanupInterval}, {"GATEWAY_AUDIO_INPUT_STORAGE_CLEANUP_LEASE", &cfg.AudioInputStorage.CleanupLease}} {
		if value, ok := lookup(setting.key); ok {
			parsed, err := time.ParseDuration(strings.TrimSpace(value))
			if err != nil {
				return fmt.Errorf("%s: must be a valid bounded duration", setting.key)
			}
			*setting.target = parsed
		}
	}
	if value, ok := lookup("GATEWAY_AUDIO_INPUT_STORAGE_ALLOWED_CONTENT_TYPES"); ok {
		cfg.AudioInputStorage.AllowedContentTypes = nil
		for _, part := range strings.Split(value, ",") {
			cfg.AudioInputStorage.AllowedContentTypes = append(cfg.AudioInputStorage.AllowedContentTypes, strings.TrimSpace(part))
		}
	}
	if err := cfg.AudioInputStorage.Validate(); err != nil {
		return fmt.Errorf("GATEWAY_AUDIO_INPUT_STORAGE_*: settings are invalid")
	}
	return nil
}

func loadSpeechOutputStorage(cfg *Config, lookup LookupEnv) error {
	if value, ok := lookup("GATEWAY_SPEECH_OUTPUT_STORAGE_MODE"); ok {
		cfg.SpeechOutputStorage.Mode = speechstorage.Mode(strings.ToLower(strings.TrimSpace(value)))
	}
	for _, setting := range []struct {
		key    string
		target *string
	}{
		{"GATEWAY_SPEECH_OUTPUT_STORAGE_ENDPOINT", &cfg.SpeechOutputStorage.Endpoint},
		{"GATEWAY_SPEECH_OUTPUT_STORAGE_REGION", &cfg.SpeechOutputStorage.Region},
		{"GATEWAY_SPEECH_OUTPUT_STORAGE_BUCKET", &cfg.SpeechOutputStorage.Bucket},
		{"GATEWAY_SPEECH_OUTPUT_STORAGE_ACCESS_KEY_ID", &cfg.SpeechOutputStorage.AccessKeyID},
		{"GATEWAY_SPEECH_OUTPUT_STORAGE_SECRET_ACCESS_KEY", &cfg.SpeechOutputStorage.SecretAccessKey},
		{"GATEWAY_SPEECH_OUTPUT_STORAGE_SERVER_SIDE_ENCRYPTION", &cfg.SpeechOutputStorage.ServerSideEncryption},
		{"GATEWAY_SPEECH_OUTPUT_STORAGE_TEMP_DIR", &cfg.SpeechOutputStorage.TemporaryDirectory},
	} {
		if value, ok := lookup(setting.key); ok {
			*setting.target = strings.TrimSpace(value)
		}
	}
	if value, ok := lookup("GATEWAY_SPEECH_OUTPUT_STORAGE_MAX_BYTES"); ok {
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return fmt.Errorf("GATEWAY_SPEECH_OUTPUT_STORAGE_MAX_BYTES: must be a valid bounded integer")
		}
		cfg.SpeechOutputStorage.MaximumBytes = parsed
	}
	if value, ok := lookup("GATEWAY_SPEECH_OUTPUT_STORAGE_MAX_CONCURRENT_CAPTURES"); ok {
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("GATEWAY_SPEECH_OUTPUT_STORAGE_MAX_CONCURRENT_CAPTURES: must be a valid bounded integer")
		}
		cfg.SpeechOutputStorage.MaximumConcurrentCaptures = parsed
	}
	for _, setting := range []struct {
		key    string
		target *time.Duration
	}{
		{"GATEWAY_SPEECH_OUTPUT_STORAGE_UPLOAD_TIMEOUT", &cfg.SpeechOutputStorage.UploadTimeout}, {"GATEWAY_SPEECH_OUTPUT_STORAGE_DOWNLOAD_TIMEOUT", &cfg.SpeechOutputStorage.DownloadTimeout}, {"GATEWAY_SPEECH_OUTPUT_STORAGE_RETENTION", &cfg.SpeechOutputStorage.Retention}, {"GATEWAY_SPEECH_OUTPUT_STORAGE_CLEANUP_INTERVAL", &cfg.SpeechOutputStorage.CleanupInterval}, {"GATEWAY_SPEECH_OUTPUT_STORAGE_CLEANUP_LEASE", &cfg.SpeechOutputStorage.CleanupLease},
	} {
		if value, ok := lookup(setting.key); ok {
			parsed, err := time.ParseDuration(strings.TrimSpace(value))
			if err != nil {
				return fmt.Errorf("%s: must be a valid bounded duration", setting.key)
			}
			*setting.target = parsed
		}
	}
	if err := cfg.SpeechOutputStorage.Validate(); err != nil {
		return fmt.Errorf("GATEWAY_SPEECH_OUTPUT_STORAGE_*: settings are invalid")
	}
	if cfg.SpeechOutputStorage.Mode == speechstorage.Managed && cfg.SpeechOutputStorage.MaximumBytes > cfg.SpeechResponseBytes {
		return fmt.Errorf("GATEWAY_SPEECH_OUTPUT_STORAGE_MAX_BYTES: must not exceed the Speech response limit")
	}
	return nil
}

func loadVideoStorage(cfg *Config, lookup LookupEnv) error {
	cfg.VideoStorage.FetchOrigins = map[string][]string{}
	if value, ok := lookup("GATEWAY_VIDEO_STORAGE_MODE"); ok {
		cfg.VideoStorage.Mode = videostorage.Mode(strings.ToLower(strings.TrimSpace(value)))
	}
	settings := []struct {
		key    string
		target *string
	}{
		{"GATEWAY_VIDEO_STORAGE_ENDPOINT", &cfg.VideoStorage.Endpoint},
		{"GATEWAY_VIDEO_STORAGE_REGION", &cfg.VideoStorage.Region},
		{"GATEWAY_VIDEO_STORAGE_BUCKET", &cfg.VideoStorage.Bucket},
		{"GATEWAY_VIDEO_STORAGE_ACCESS_KEY_ID", &cfg.VideoStorage.AccessKeyID},
		{"GATEWAY_VIDEO_STORAGE_SECRET_ACCESS_KEY", &cfg.VideoStorage.SecretAccessKey},
		{"GATEWAY_VIDEO_STORAGE_CDN_BASE_URL", &cfg.VideoStorage.CDNBaseURL},
		{"GATEWAY_VIDEO_STORAGE_TEMP_DIR", &cfg.VideoStorage.TemporaryDirectory},
	}
	for _, setting := range settings {
		if value, ok := lookup(setting.key); ok {
			*setting.target = strings.TrimSpace(value)
		}
	}
	integers := []struct {
		key    string
		target *int64
	}{
		{"GATEWAY_VIDEO_STORAGE_MAX_VIDEO_BYTES", &cfg.VideoStorage.MaximumVideoBytes},
		{"GATEWAY_VIDEO_STORAGE_MAX_TOTAL_BYTES", &cfg.VideoStorage.MaximumTotalBytes},
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
	counts := []struct {
		key    string
		target *int
	}{
		{"GATEWAY_VIDEO_STORAGE_MAX_VIDEOS", &cfg.VideoStorage.MaximumVideos},
		{"GATEWAY_VIDEO_STORAGE_MAX_CONCURRENT_DOWNLOADS", &cfg.VideoStorage.MaximumConcurrentDownloads},
	}
	for _, setting := range counts {
		if value, ok := lookup(setting.key); ok {
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return fmt.Errorf("%s: must be a valid bounded integer", setting.key)
			}
			*setting.target = parsed
		}
	}
	durations := []struct {
		key    string
		target *time.Duration
	}{
		{"GATEWAY_VIDEO_STORAGE_FETCH_TIMEOUT", &cfg.VideoStorage.FetchTimeout},
		{"GATEWAY_VIDEO_STORAGE_UPLOAD_TIMEOUT", &cfg.VideoStorage.UploadTimeout},
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
	if value, ok := lookup("GATEWAY_VIDEO_STORAGE_FETCH_ORIGINS_RUNWAY"); ok {
		for _, part := range strings.Split(value, ",") {
			cfg.VideoStorage.FetchOrigins["runway"] = append(cfg.VideoStorage.FetchOrigins["runway"], strings.TrimSpace(part))
		}
	}
	if err := cfg.VideoStorage.Validate(); err != nil {
		return fmt.Errorf("GATEWAY_VIDEO_STORAGE_*: settings are invalid")
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

func loadPlugins(cfg *Config, lookup LookupEnv) error {
	if value, ok := lookup("GATEWAY_PLUGIN_MODE"); ok {
		cfg.PluginMode = PluginMode(strings.TrimSpace(value))
	}
	if cfg.PluginMode != PluginDisabled && cfg.PluginMode != PluginOptional && cfg.PluginMode != PluginRequired {
		return fmt.Errorf("GATEWAY_PLUGIN_MODE: must be disabled, optional, or required")
	}
	if value, ok := lookup("GATEWAY_PLUGIN_MANIFEST_DIR"); ok {
		cfg.PluginManifestDir = strings.TrimSpace(value)
	}
	if value, ok := lookup("GATEWAY_PLUGIN_REGISTRY_MODE"); ok {
		cfg.PluginRegistryMode = PluginRegistryMode(strings.TrimSpace(value))
	}
	if cfg.PluginRegistryMode != PluginRegistryDisabled && cfg.PluginRegistryMode != PluginRegistryRequired {
		return fmt.Errorf("GATEWAY_PLUGIN_REGISTRY_MODE: must be disabled or required")
	}
	for key, target := range map[string]*string{"GATEWAY_PLUGIN_REGISTRY_TRUST_FILE": &cfg.PluginRegistryTrustFile, "GATEWAY_PLUGIN_REGISTRY_INDEX_FILE": &cfg.PluginRegistryIndexFile, "GATEWAY_PLUGIN_REGISTRY_ADMISSION_DIR": &cfg.PluginRegistryAdmissionDir, "GATEWAY_PLUGIN_REGISTRY_PLATFORM": &cfg.PluginRegistryPlatform} {
		if value, ok := lookup(key); ok {
			*target = strings.TrimSpace(value)
		}
	}
	if value, ok := lookup("GATEWAY_PLUGIN_REGISTRY_MINIMUM_SEQUENCE"); ok {
		parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil || parsed < 1 {
			return fmt.Errorf("GATEWAY_PLUGIN_REGISTRY_MINIMUM_SEQUENCE: must be a positive integer")
		}
		cfg.PluginRegistryMinimumSequence = parsed
	}
	if value, ok := lookup("GATEWAY_PLUGIN_ENDPOINTS_JSON"); ok {
		if err := decodePluginJSON(value, &cfg.Plugins.EndpointOrigins); err != nil || len(cfg.Plugins.EndpointOrigins) > 128 {
			return fmt.Errorf("GATEWAY_PLUGIN_ENDPOINTS_JSON: must be a bounded JSON object")
		}
	}
	if value, ok := lookup("GATEWAY_PLUGIN_AUTH_SECRET_ENV_JSON"); ok {
		var references map[string]string
		if err := decodePluginJSON(value, &references); err != nil || len(references) > 128 {
			return fmt.Errorf("GATEWAY_PLUGIN_AUTH_SECRET_ENV_JSON: must be a bounded JSON object")
		}
		for reference, environmentName := range references {
			if !validPluginReference(reference) || !validEnvironmentName(environmentName) {
				return fmt.Errorf("GATEWAY_PLUGIN_AUTH_SECRET_ENV_JSON: contains an invalid reference")
			}
			secret, exists := lookup(environmentName)
			if !exists || secret == "" || len(secret) > 4096 || strings.TrimSpace(secret) != secret {
				return fmt.Errorf("GATEWAY_PLUGIN_AUTH_SECRET_ENV_JSON: referenced secret is unavailable")
			}
			cfg.Plugins.AuthSecrets[reference] = secret
		}
	}
	if value, ok := lookup("GATEWAY_PLUGIN_RESULT_ORIGINS_JSON"); ok {
		if err := decodePluginJSON(value, &cfg.Plugins.ResultOrigins); err != nil || len(cfg.Plugins.ResultOrigins) > 128 {
			return fmt.Errorf("GATEWAY_PLUGIN_RESULT_ORIGINS_JSON: must be a bounded JSON object")
		}
		var combined []string
		for _, origins := range cfg.Plugins.ResultOrigins {
			combined = append(combined, origins...)
		}
		if len(combined) > 32 {
			return fmt.Errorf("GATEWAY_PLUGIN_RESULT_ORIGINS_JSON: must contain no more than 32 origins")
		}
		cfg.ImageStorage.FetchOrigins["plugin"] = combined
		if cfg.ImageStorage.Validate() != nil {
			return fmt.Errorf("GATEWAY_PLUGIN_RESULT_ORIGINS_JSON: contains an invalid origin")
		}
	}
	if value, ok := lookup("GATEWAY_PLUGIN_TIMEOUT"); ok {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration <= 0 || duration > 5*time.Minute {
			return fmt.Errorf("GATEWAY_PLUGIN_TIMEOUT: must be a positive duration no greater than 5m")
		}
		cfg.Plugins.Timeout = duration
	}
	byteSettings := []struct {
		key     string
		target  *int64
		maximum int64
	}{{"GATEWAY_PLUGIN_MAX_REQUEST_BYTES", &cfg.Plugins.MaximumRequestBytes, 64 << 20}, {"GATEWAY_PLUGIN_MAX_RESPONSE_BYTES", &cfg.Plugins.MaximumResponseBytes, 128 << 20}}
	for _, setting := range byteSettings {
		if value, ok := lookup(setting.key); ok {
			parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil || parsed < 1 || parsed > setting.maximum {
				return fmt.Errorf("%s: must be a positive bounded integer", setting.key)
			}
			*setting.target = parsed
		}
	}
	if value, ok := lookup("GATEWAY_PLUGIN_MAX_CONCURRENCY"); ok {
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || parsed < 1 || parsed > 4096 {
			return fmt.Errorf("GATEWAY_PLUGIN_MAX_CONCURRENCY: must be between 1 and 4096")
		}
		cfg.Plugins.MaximumConcurrency = parsed
	}
	if cfg.PluginMode != PluginDisabled && (cfg.PluginManifestDir == "" || len(cfg.Plugins.EndpointOrigins) == 0 || len(cfg.Plugins.AuthSecrets) == 0) {
		return fmt.Errorf("GATEWAY_PLUGIN_*: enabled plugin mode requires manifest directory, endpoints, and auth secret references")
	}
	if cfg.PluginRegistryMode == PluginRegistryRequired && (cfg.PluginMode == PluginDisabled || cfg.PluginRegistryTrustFile == "" || cfg.PluginRegistryIndexFile == "" || cfg.PluginRegistryAdmissionDir == "") {
		return fmt.Errorf("GATEWAY_PLUGIN_REGISTRY_*: required registry mode requires enabled plugins and local trust, index, and admission paths")
	}
	return nil
}

func decodePluginJSON(raw string, target any) error {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return fmt.Errorf("invalid JSON")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}

func validPluginReference(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || index > 0 && (character == '.' || character == '_' || character == '-')) {
			return false
		}
	}
	return true
}
func validEnvironmentName(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if !(character >= 'A' && character <= 'Z' || character == '_' || index > 0 && character >= '0' && character <= '9') {
			return false
		}
	}
	return true
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
