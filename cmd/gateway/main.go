package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/app"
	"github.com/nativegatewayhq/gateway/internal/audioassets"
	"github.com/nativegatewayhq/gateway/internal/audiobilling"
	"github.com/nativegatewayhq/gateway/internal/audiopricing"
	"github.com/nativegatewayhq/gateway/internal/audioreconciliation"
	chargebilling "github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/chatbilling"
	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	"github.com/nativegatewayhq/gateway/internal/chatreconciliation"
	"github.com/nativegatewayhq/gateway/internal/clientip"
	"github.com/nativegatewayhq/gateway/internal/config"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/imagestorage"
	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/networkauth"
	"github.com/nativegatewayhq/gateway/internal/observability"
	"github.com/nativegatewayhq/gateway/internal/plugins"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/ratelimit"
	"github.com/nativegatewayhq/gateway/internal/reconciliation"
	"github.com/nativegatewayhq/gateway/internal/runwayassets"
	"github.com/nativegatewayhq/gateway/internal/speechstorage"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	"github.com/nativegatewayhq/gateway/internal/videostorage"
	anthropicoperation "github.com/nativegatewayhq/gateway/operations/anthropic"
	audiooperation "github.com/nativegatewayhq/gateway/operations/audio"
	chatoperation "github.com/nativegatewayhq/gateway/operations/chat"
	geminioperation "github.com/nativegatewayhq/gateway/operations/gemini"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	responsesoperation "github.com/nativegatewayhq/gateway/operations/responses"
	videooperation "github.com/nativegatewayhq/gateway/operations/video"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	registryv1 "github.com/nativegatewayhq/gateway/plugin-sdk/registry/v1"
	anthropicProtocol "github.com/nativegatewayhq/gateway/protocols/anthropic"
	falProtocol "github.com/nativegatewayhq/gateway/protocols/fal"
	"github.com/nativegatewayhq/gateway/protocols/gemini"
	managementProtocol "github.com/nativegatewayhq/gateway/protocols/management"
	openaiProtocol "github.com/nativegatewayhq/gateway/protocols/openai"
	replicateProtocol "github.com/nativegatewayhq/gateway/protocols/replicate"
	runwayProtocol "github.com/nativegatewayhq/gateway/protocols/runway"
	anthropicProvider "github.com/nativegatewayhq/gateway/providers/anthropic"
	falProvider "github.com/nativegatewayhq/gateway/providers/fal"
	"github.com/nativegatewayhq/gateway/providers/google"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
	pluginProvider "github.com/nativegatewayhq/gateway/providers/plugin"
	replicateProvider "github.com/nativegatewayhq/gateway/providers/replicate"
	runwayProvider "github.com/nativegatewayhq/gateway/providers/runway"
	"github.com/nativegatewayhq/gateway/providers/xai"
)

func main() {
	os.Exit(run(os.Stdout, os.Stderr))
}

func run(stdout, stderr io.Writer) int {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "configuration error: %v\n", err)
		return 1
	}
	providerCredentialRegistry, err := providercredentials.Load(os.LookupEnv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "provider credential configuration error: %v\n", err)
		return 1
	}
	providerCredentialKeyring, err := providercredentials.LoadMasterKeyring(os.LookupEnv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "provider credential key configuration error")
		return 1
	}

	logger := observability.NewLogger(stdout, cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	telemetryRuntime, err := telemetry.New(ctx, cfg.Telemetry)
	if err != nil {
		logger.Error("gateway telemetry initialization failed")
		return 1
	}
	defer func() {
		if shutdownErr := telemetryRuntime.Shutdown(context.Background()); shutdownErr != nil {
			logger.Warn("gateway telemetry shutdown incomplete", "category", "telemetry_shutdown_failed")
		}
	}()
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("gateway database unavailable")
		return 1
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		logger.Error("gateway database migration failed")
		return 1
	}
	if providerCredentialKeyring != nil {
		providerCredentialRegistry, err = providercredentials.NewControlPlane(providerCredentialRegistry, providercredentials.NewStore(pool, providerCredentialKeyring))
		if err != nil {
			logger.Error("provider credential control plane initialization failed")
			return 1
		}
	}
	var apiKeyAuthenticator gemini.Authenticator = apikey.NewService(apikey.NewPostgresStore(pool))
	networkGuard, err := networkauth.NewGuardedAuthenticator(apiKeyAuthenticator)
	if err != nil {
		logger.Error("gateway network policy initialization failed")
		return 1
	}
	apiKeyAuthenticator = networkGuard
	readinessChecks := []func(context.Context) error{pool.Ping}
	var audioAssetService *audioassets.Service
	var openAIAudioAssetHandler http.Handler
	if cfg.AudioInputStorage.Mode == audioassets.Managed {
		repository, storageErr := audioassets.NewRepository(pool)
		if storageErr != nil {
			logger.Error("gateway audio asset initialization failed")
			return 1
		}
		objects, storageErr := audioassets.NewS3(audioassets.S3Config{Endpoint: cfg.AudioInputStorage.Endpoint, Region: cfg.AudioInputStorage.Region, Bucket: cfg.AudioInputStorage.Bucket, AccessKeyID: cfg.AudioInputStorage.AccessKeyID, SecretAccessKey: cfg.AudioInputStorage.SecretAccessKey, ServerSideEncryption: cfg.AudioInputStorage.ServerSideEncryption, UploadTimeout: cfg.AudioInputStorage.UploadTimeout, DownloadTimeout: cfg.AudioInputStorage.DownloadTimeout})
		if storageErr != nil {
			logger.Error("gateway audio asset initialization failed")
			return 1
		}
		audioAssetService, storageErr = audioassets.NewService(repository, objects, cfg.AudioInputStorage.Retention, cfg.AudioInputStorage.CleanupLease, cfg.AudioInputStorage.MaximumBytes, fmt.Sprintf("gateway-audio-asset-%d", os.Getpid()))
		if storageErr != nil {
			logger.Error("gateway audio asset initialization failed")
			return 1
		}
		audioAssetService.SetTelemetry(telemetryRuntime.Recorder)
		readinessChecks = append(readinessChecks, audioAssetService.Ready)
	}
	var speechOutputService *speechstorage.Service
	var openAISpeechAssetHandler http.Handler
	if cfg.SpeechOutputStorage.Mode == speechstorage.Managed {
		repository, storageErr := speechstorage.NewRepository(pool)
		if storageErr != nil {
			logger.Error("gateway speech output initialization failed")
			return 1
		}
		objects, storageErr := audioassets.NewS3(audioassets.S3Config{Endpoint: cfg.SpeechOutputStorage.Endpoint, Region: cfg.SpeechOutputStorage.Region, Bucket: cfg.SpeechOutputStorage.Bucket, AccessKeyID: cfg.SpeechOutputStorage.AccessKeyID, SecretAccessKey: cfg.SpeechOutputStorage.SecretAccessKey, ServerSideEncryption: cfg.SpeechOutputStorage.ServerSideEncryption, UploadTimeout: cfg.SpeechOutputStorage.UploadTimeout, DownloadTimeout: cfg.SpeechOutputStorage.DownloadTimeout})
		if storageErr != nil {
			logger.Error("gateway speech output initialization failed")
			return 1
		}
		speechOutputService, storageErr = speechstorage.NewService(repository, objects, cfg.SpeechOutputStorage, fmt.Sprintf("gateway-speech-output-%d", os.Getpid()))
		if storageErr != nil {
			logger.Error("gateway speech output initialization failed")
			return 1
		}
		speechOutputService.SetTelemetry(telemetryRuntime.Recorder)
		openAISpeechAssetHandler = openaiProtocol.NewSpeechAssetHandler(logger, apiKeyAuthenticator, speechOutputService)
		readinessChecks = append(readinessChecks, speechOutputService.Ready)
	}
	var imageResults *imagestorage.Manager
	if cfg.ImageStorage.Mode == imagestorage.Managed {
		collector, storageErr := imagestorage.NewCollector(cfg.ImageStorage)
		if storageErr != nil {
			logger.Error("gateway image storage initialization failed")
			return 1
		}
		objects, storageErr := imagestorage.NewS3(cfg.ImageStorage)
		if storageErr != nil {
			logger.Error("gateway image storage initialization failed")
			return 1
		}
		imageResults, storageErr = imagestorage.NewManager(cfg.ImageStorage, collector, objects, imagestorage.NewAssetStore(pool))
		if storageErr != nil {
			logger.Error("gateway image storage initialization failed")
			return 1
		}
		readinessChecks = append(readinessChecks, objects.Ready)
		imageResults.SetTelemetry(telemetryRuntime.Recorder)
	}
	var videoResults *videostorage.Manager
	if cfg.VideoStorage.Mode == videostorage.Managed {
		collector, storageErr := videostorage.NewCollector(cfg.VideoStorage)
		if storageErr != nil {
			logger.Error("gateway video storage initialization failed")
			return 1
		}
		objects, storageErr := imagestorage.NewS3(cfg.VideoStorage.ImageObjectConfig())
		if storageErr != nil {
			logger.Error("gateway video storage initialization failed")
			return 1
		}
		repository, storageErr := videostorage.NewRepository(pool)
		if storageErr != nil {
			logger.Error("gateway video storage initialization failed")
			return 1
		}
		videoResults, storageErr = videostorage.NewManager(cfg.VideoStorage, collector, objects, repository)
		if storageErr != nil {
			logger.Error("gateway video storage initialization failed")
			return 1
		}
		readinessChecks = append(readinessChecks, videoResults.Ready)
	}
	var redisLimiter *ratelimit.RedisLimiter
	if cfg.RateLimitMode == config.RateLimitRequired {
		redisLimiter, err = ratelimit.NewRedis(cfg.RedisURL, cfg.RateLimitTimeout)
		if err != nil {
			logger.Error("gateway rate limiter initialization failed")
			return 1
		}
		defer redisLimiter.Close()
		guarded, guardErr := ratelimit.NewGuardedAuthenticator(apiKeyAuthenticator, redisLimiter)
		if guardErr != nil {
			logger.Error("gateway rate limiter initialization failed")
			return 1
		}
		apiKeyAuthenticator = guarded
		readinessChecks = append(readinessChecks, redisLimiter.Ping)
	}
	var healthGate providerhealth.Gate = providerhealth.NoopGate{}
	if cfg.ProviderHealthMode == config.ProviderHealthRequired {
		redisHealth, healthErr := providerhealth.NewRedis(cfg.RedisURL, cfg.ProviderHealth)
		if healthErr != nil {
			logger.Error("gateway provider health initialization failed")
			return 1
		}
		defer redisHealth.Close()
		healthGate = redisHealth
		readinessChecks = append(readinessChecks, redisHealth.Ping)
	}
	ready := func(ctx context.Context) error {
		for _, check := range readinessChecks {
			if checkErr := check(ctx); checkErr != nil {
				return checkErr
			}
		}
		return nil
	}
	googleExecutor := google.New(providerCredentialRegistry, cfg.GoogleTimeout, cfg.GeminiStreamIdleTimeout)
	var geminiExecutor gemini.Executor = googleExecutor
	var pluginRegistry *plugins.Registry
	var pluginClient *plugins.Client
	var pluginImageExecutor *pluginProvider.Executor
	var pluginRoutes []imageoperation.ModelRoute
	var pluginVideoRoutes []videooperation.Route
	if cfg.PluginMode != config.PluginDisabled {
		validated, loadErr := manifest.LoadDirectory(cfg.PluginManifestDir, "0.1.0")
		if loadErr != nil || cfg.PluginMode == config.PluginRequired && len(validated) == 0 {
			logger.Error("gateway plugin manifest initialization failed")
			return 1
		}
		pluginStore := plugins.NewStore(pool)
		if cfg.PluginRegistryMode == config.PluginRegistryRequired {
			lastSequence, lastDigest, stateErr := pluginStore.LastRegistryIndex(ctx)
			if stateErr != nil {
				logger.Error("gateway plugin registry state initialization failed")
				return 1
			}
			snapshot, registryErr := registryv1.LoadSnapshot(registryv1.BundleConfig{TrustPolicyFile: cfg.PluginRegistryTrustFile, IndexEnvelopeFile: cfg.PluginRegistryIndexFile, AdmissionDirectory: cfg.PluginRegistryAdmissionDir, GatewayVersion: "0.1.0", Platform: cfg.PluginRegistryPlatform, MinimumSequence: cfg.PluginRegistryMinimumSequence, LastSequence: lastSequence, LastIndexDigest: lastDigest, Now: time.Now().UTC().Truncate(time.Second)}, validated)
			if registryErr != nil {
				logger.Error("gateway signed plugin registry initialization failed")
				return 1
			}
			pluginRegistry, err = plugins.NewAdmittedRegistry(validated, cfg.Plugins, snapshot)
		} else {
			pluginRegistry, err = plugins.NewRegistry(validated, cfg.Plugins)
		}
		if err != nil {
			logger.Error("gateway plugin registry initialization failed")
			return 1
		}
		if err = pluginStore.Sync(ctx, pluginRegistry); err != nil {
			logger.Error("gateway plugin channel snapshot initialization failed")
			return 1
		}
		pluginClient = plugins.NewClient(pluginRegistry)
		if cfg.PluginMode == config.PluginRequired {
			readinessChecks = append(readinessChecks, pluginClient.Health)
		}
		pluginImageExecutor = pluginProvider.New(pluginClient)
		geminiExecutor = pluginProvider.NewGeminiMux(pluginRegistry, pluginImageExecutor, googleExecutor)
		pluginRoutes = pluginRegistry.Routes()
		pluginVideoRoutes = pluginRegistry.VideoRoutes()
	}
	imageModels, err := imageoperation.DefaultRegistryWithAsyncAndAdditional(cfg.ReplicateModels, cfg.FalModels, pluginRoutes)
	if err != nil {
		logger.Error("gateway model registry initialization failed")
		return 1
	}
	videoModels, err := videooperation.NewRegistryWithCapabilitiesAndAdditional(cfg.RunwayModels, cfg.RunwayModelCapabilities, pluginVideoRoutes)
	if err != nil {
		logger.Error("gateway video model registry initialization failed")
		return 1
	}
	geminiLimits := make(map[string]geminioperation.Limits, len(cfg.GeminiLLMModelLimits))
	for model, limit := range cfg.GeminiLLMModelLimits {
		geminiLimits[model] = geminioperation.Limits{MaximumInputTokens: limit.MaximumInputTokens, MaximumOutputTokens: limit.MaximumOutputTokens}
	}
	geminiLLMModels, err := geminioperation.NewRegistryWithLimits(cfg.GeminiLLMModels, geminiLimits)
	if err != nil {
		logger.Error("gateway Gemini LLM model registry initialization failed")
		return 1
	}
	openAIExecutor := openaiProvider.New(providerCredentialRegistry, cfg.ImagesTimeout)
	chatLimits := make(map[string]chatoperation.Limits, len(cfg.OpenAIChatModelLimits))
	for model, limit := range cfg.OpenAIChatModelLimits {
		chatLimits[model] = chatoperation.Limits{MaximumInputTokens: limit.MaximumInputTokens, MaximumOutputTokens: limit.MaximumOutputTokens}
	}
	chatModels, err := chatoperation.NewRegistryWithLimits(cfg.OpenAIChatModels, chatLimits)
	if len(cfg.OpenAIChatRoutes) > 0 {
		routes := make([]chatoperation.Route, 0, len(cfg.OpenAIChatRoutes))
		for _, configured := range cfg.OpenAIChatRoutes {
			candidates := make([]chatoperation.Candidate, 0, len(configured.Candidates))
			for _, candidate := range configured.Candidates {
				provider, providerErr := providercredentials.ParseProviderID(candidate.Provider)
				if providerErr != nil {
					err = providerErr
					break
				}
				candidates = append(candidates, chatoperation.Candidate{ID: candidate.ID, Provider: provider, ProviderModel: candidate.ProviderModel, ChannelID: candidate.ChannelID, Priority: candidate.Priority, Weight: candidate.Weight, Enabled: candidate.Enabled, Capabilities: chatoperation.Capabilities{Streaming: candidate.Streaming, Tools: candidate.Tools, JSONMode: candidate.JSONMode}})
			}
			if err != nil {
				break
			}
			routes = append(routes, chatoperation.Route{Model: configured.Model, Owner: configured.Owner, Policy: chatoperation.Policy(configured.Policy), FixedCandidateID: configured.FixedCandidateID, MaximumInputTokens: configured.MaximumInputTokens, MaximumOutputTokens: configured.MaximumOutputTokens, Candidates: candidates})
		}
		if err == nil {
			chatModels, err = chatoperation.NewRouteRegistry(routes)
		}
	}
	if err != nil {
		logger.Error("gateway chat model registry initialization failed")
		return 1
	}
	responsesLimits := make(map[string]responsesoperation.Limits, len(cfg.OpenAIResponsesModelLimits))
	for model, limit := range cfg.OpenAIResponsesModelLimits {
		responsesLimits[model] = responsesoperation.Limits{MaximumInputTokens: limit.MaximumInputTokens, MaximumOutputTokens: limit.MaximumOutputTokens}
	}
	responsesModels, err := responsesoperation.NewRegistryWithLimits(cfg.OpenAIResponsesModels, responsesLimits)
	if len(cfg.OpenAIResponsesRoutes) > 0 {
		routes := make([]responsesoperation.Route, 0, len(cfg.OpenAIResponsesRoutes))
		for _, configured := range cfg.OpenAIResponsesRoutes {
			candidates := make([]responsesoperation.Candidate, 0, len(configured.Candidates))
			for _, candidate := range configured.Candidates {
				provider, providerErr := providercredentials.ParseProviderID(candidate.Provider)
				if providerErr != nil {
					err = providerErr
					break
				}
				candidates = append(candidates, responsesoperation.Candidate{ID: candidate.ID, Provider: provider, ProviderModel: candidate.ProviderModel, ChannelID: candidate.ChannelID, Priority: candidate.Priority, Weight: candidate.Weight, Enabled: candidate.Enabled, Capabilities: responsesoperation.Capabilities{Streaming: candidate.Streaming, FunctionTools: candidate.FunctionTools, WebSearch: candidate.WebSearch, XSearch: candidate.XSearch, CodeInterpreter: candidate.CodeInterpreter, ImageGeneration: candidate.ImageGeneration, JSONMode: candidate.JSONMode, StoredResponse: candidate.StoredResponse}})
			}
			if err != nil {
				break
			}
			routes = append(routes, responsesoperation.Route{Model: configured.Model, Owner: configured.Owner, Policy: responsesoperation.Policy(configured.Policy), FixedCandidateID: configured.FixedCandidateID, MaximumInputTokens: configured.MaximumInputTokens, MaximumOutputTokens: configured.MaximumOutputTokens, Candidates: candidates})
		}
		if err == nil {
			responsesModels, err = responsesoperation.NewRouteRegistry(routes)
		}
	}
	if err != nil {
		logger.Error("gateway Responses model registry initialization failed")
		return 1
	}
	audioModels, err := audiooperation.NewRegistry(cfg.OpenAISpeechModels)
	if err != nil {
		logger.Error("gateway Audio Speech model registry initialization failed")
		return 1
	}
	transcriptionModels, err := audiooperation.NewTranscriptionRegistry(cfg.OpenAITranscriptionModels, cfg.OpenAITranscriptionCapabilities)
	if err != nil {
		logger.Error("gateway Audio Transcription model registry initialization failed")
		return 1
	}
	translationModels, err := audiooperation.NewTranslationRegistry(cfg.OpenAITranslationModels, cfg.OpenAITranslationModelMap, cfg.OpenAITranslationCapabilities)
	if err != nil {
		logger.Error("gateway Audio Translation model registry initialization failed")
		return 1
	}
	anthropicLimits := make(map[string]anthropicoperation.Limits, len(cfg.AnthropicMessagesModelLimits))
	for model, limit := range cfg.AnthropicMessagesModelLimits {
		anthropicLimits[model] = anthropicoperation.Limits{MaximumInputTokens: limit.MaximumInputTokens, MaximumOutputTokens: limit.MaximumOutputTokens}
	}
	anthropicModels, err := anthropicoperation.NewRegistryWithLimits(cfg.AnthropicMessagesModels, anthropicLimits)
	if err != nil {
		logger.Error("gateway Anthropic model registry initialization failed")
		return 1
	}
	var anthropicHandler http.Handler
	var openAIChatHandler http.Handler
	var openAIResponsesHandler http.Handler
	var openAISpeechHandler http.Handler
	var openAITranscriptionHandler http.Handler
	var openAITranslationHandler http.Handler
	if audioAssetService != nil {
		openAIAudioAssetHandler = openaiProtocol.NewAudioAssetHandler(logger, apiKeyAuthenticator, audioAssetService, cfg.AudioInputStorage.MaximumBytes, cfg.AudioInputStorage.MaximumConcurrentUploads, cfg.AudioInputStorage.AllowedContentTypes, cfg.AudioInputStorage.TemporaryDirectory)
	}
	xAIExecutor := xai.New(providerCredentialRegistry, cfg.ImagesTimeout)
	imageExecutors := map[providercredentials.ProviderID]openaiProtocol.Executor{
		providercredentials.OpenAI: openAIExecutor,
		providercredentials.XAI:    xAIExecutor,
	}
	if pluginImageExecutor != nil {
		imageExecutors[providercredentials.Plugin] = pluginImageExecutor
	}
	providerAvailability := plugins.NewAvailability(providerCredentialRegistry, pluginRegistry)
	var chargeBilling openaiProtocol.Billing
	var billingService *chargebilling.Service
	var reconciliationWorker *reconciliation.Worker
	var chatChargeBilling openaiProtocol.ChatBilling
	var responsesChargeBilling openaiProtocol.ResponsesBilling
	var anthropicChargeBilling anthropicProtocol.Billing
	var audioChargeBilling openaiProtocol.SpeechBilling
	var transcriptionChargeBilling openaiProtocol.TranscriptionBilling
	var translationChargeBilling openaiProtocol.TranslationBilling
	var transcriptionPricing *audiopricing.Service
	var translationPricing *audiopricing.Service
	var chatReconciliationWorker *chatreconciliation.Worker
	var transcriptionReconciliationWorker *audioreconciliation.Worker
	var translationReconciliationWorker *audioreconciliation.TranslationWorker
	if cfg.BillingMode == config.BillingRequired {
		priceEstimator, pricingErr := pricing.NewService(pool, cfg.MinimumMarginBPS)
		if pricingErr != nil {
			logger.Error("gateway pricing initialization failed")
			return 1
		}
		var billingErr error
		billingService, billingErr = chargebilling.NewServiceWithControls(pool, priceEstimator, ledger.NewService(pool), costquota.NewStore(pool), spendcap.NewStore(pool), cfg.ReplayBodyBytes)
		if billingErr != nil {
			logger.Error("gateway billing initialization failed")
			return 1
		}
		chargeBilling = billingService
		chatPrices, chatPriceErr := chatpricing.New(pool, cfg.MinimumMarginBPS)
		if chatPriceErr != nil {
			logger.Error("gateway Chat pricing initialization failed")
			return 1
		}
		chatService, chatBillingErr := chatbilling.NewWithControls(pool, chatPrices, ledger.NewService(pool), costquota.NewStore(pool), spendcap.NewStore(pool), cfg.ReplayBodyBytes)
		if chatBillingErr != nil {
			logger.Error("gateway Chat billing initialization failed")
			return 1
		}
		chatChargeBilling = chatService
		responsesChargeBilling = chatService
		anthropicChargeBilling = chatService
		audioPrices, audioPriceErr := audiopricing.New(pool, cfg.MinimumMarginBPS)
		if audioPriceErr != nil {
			logger.Error("gateway Audio pricing initialization failed")
			return 1
		}
		audioService, audioBillingErr := audiobilling.NewWithControls(pool, audioPrices, ledger.NewService(pool), costquota.NewStore(pool), spendcap.NewStore(pool))
		if audioBillingErr != nil {
			logger.Error("gateway Audio billing initialization failed")
			return 1
		}
		audioChargeBilling = audioService
		transcriptionPricing = audioPrices
		translationPricing = audioPrices
		if len(cfg.OpenAITranscriptionModels) > 0 {
			readinessChecks = append(readinessChecks, func(ctx context.Context) error {
				for _, model := range transcriptionModels.List() {
					if _, priceErr := audioPrices.EstimateTranscription(ctx, audiopricing.TranscriptionPriceRequest{ChannelID: model.ChannelID, Model: model.ID}); priceErr != nil {
						return priceErr
					}
				}
				return nil
			})
		}
		transcriptionService, transcriptionBillingErr := audiobilling.NewTranscriptionWithControls(pool, audioPrices, ledger.NewService(pool), costquota.NewStore(pool), spendcap.NewStore(pool))
		if transcriptionBillingErr != nil {
			logger.Error("gateway Audio Transcription billing initialization failed")
			return 1
		}
		transcriptionChargeBilling = transcriptionService
		transcriptionReconciliationWorker, transcriptionBillingErr = audioreconciliation.New(pool, transcriptionService, fmt.Sprintf("gateway-transcription-%d", os.Getpid()), cfg.ReconcileLease, cfg.ReconcileMaxAttempts)
		if transcriptionBillingErr != nil {
			logger.Error("gateway Audio Transcription reconciliation initialization failed")
			return 1
		}
		if len(cfg.OpenAITranslationModels) > 0 {
			readinessChecks = append(readinessChecks, func(ctx context.Context) error {
				for _, model := range translationModels.List() {
					if _, priceErr := audioPrices.EstimateTranslation(ctx, audiopricing.TranslationPriceRequest{ChannelID: model.ChannelID, Model: model.ID}); priceErr != nil {
						return priceErr
					}
				}
				return nil
			})
		}
		translationService, translationBillingErr := audiobilling.NewTranslationWithControls(pool, audioPrices, ledger.NewService(pool), costquota.NewStore(pool), spendcap.NewStore(pool))
		if translationBillingErr != nil {
			logger.Error("gateway Audio Translation billing initialization failed")
			return 1
		}
		translationChargeBilling = translationService
		translationReconciliationWorker, translationBillingErr = audioreconciliation.NewTranslation(pool, translationService, fmt.Sprintf("gateway-translation-%d", os.Getpid()), cfg.ReconcileLease, cfg.ReconcileMaxAttempts)
		if translationBillingErr != nil {
			logger.Error("gateway Audio Translation reconciliation initialization failed")
			return 1
		}
		chatReconciliationWorker, chatBillingErr = chatreconciliation.New(pool, chatService, fmt.Sprintf("gateway-%d", os.Getpid()), cfg.ReconcileLease, cfg.ReconcileMaxAttempts)
		if chatBillingErr != nil {
			logger.Error("gateway Chat reconciliation initialization failed")
			return 1
		}
		reconciliationConfig := reconciliation.Config{
			Interval: cfg.ReconcileInterval, Lease: cfg.ReconcileLease,
			BaseBackoff: cfg.ReconcileBackoff, MaxBackoff: cfg.ReconcileMaxBackoff,
			BatchSize: cfg.ReconcileBatchSize, MaxAttempts: cfg.ReconcileMaxAttempts,
		}
		if imageResults != nil {
			reconciliationWorker, billingErr = reconciliation.NewWithResultManager(pool, billingService, reconciliationConfig, imageResults)
		} else {
			reconciliationWorker, billingErr = reconciliation.New(pool, billingService, reconciliationConfig)
		}
		if billingErr != nil {
			logger.Error("gateway reconciliation initialization failed")
			return 1
		}
	}
	if len(cfg.OpenAIChatModels) > 0 {
		chatExecutors := map[providercredentials.ProviderID]openaiProtocol.ChatExecutor{
			providercredentials.OpenAI: openaiProvider.NewChat(providerCredentialRegistry, cfg.ChatTimeout, cfg.ChatStreamIdleTimeout),
			providercredentials.XAI:    xai.NewChat(providerCredentialRegistry, cfg.ChatTimeout, cfg.ChatStreamIdleTimeout),
		}
		var chatHandler *openaiProtocol.ChatHandler
		if chatChargeBilling == nil {
			chatHandler = openaiProtocol.NewRoutedChatHandler(logger, apiKeyAuthenticator, chatModels, chatExecutors, providerCredentialRegistry, healthGate, cfg.ChatBodyBytes)
		} else {
			chatHandler = openaiProtocol.NewBillableRoutedChatHandler(logger, apiKeyAuthenticator, chatModels, chatExecutors, providerCredentialRegistry, healthGate, cfg.ChatBodyBytes, chatChargeBilling)
		}
		chatHandler.SetTelemetry(telemetryRuntime.Recorder)
		openAIChatHandler = chatHandler
	}
	if len(cfg.OpenAIResponsesModels) > 0 {
		responsesExecutors := map[providercredentials.ProviderID]openaiProtocol.ResponsesExecutor{
			providercredentials.OpenAI: openaiProvider.NewResponses(providerCredentialRegistry, cfg.ResponsesTimeout, cfg.ResponsesStreamIdleTimeout),
			providercredentials.XAI:    xai.NewResponses(providerCredentialRegistry, cfg.ResponsesTimeout, cfg.ResponsesStreamIdleTimeout),
		}
		var responsesHandler *openaiProtocol.ResponsesHandler
		if responsesChargeBilling == nil {
			responsesHandler = openaiProtocol.NewRoutedResponsesHandler(logger, apiKeyAuthenticator, responsesModels, responsesExecutors, providerCredentialRegistry, healthGate, cfg.ResponsesBodyBytes)
		} else {
			responsesHandler = openaiProtocol.NewBillableRoutedResponsesHandler(logger, apiKeyAuthenticator, responsesModels, responsesExecutors, providerCredentialRegistry, healthGate, cfg.ResponsesBodyBytes, responsesChargeBilling)
		}
		responsesHandler.SetTelemetry(telemetryRuntime.Recorder)
		openAIResponsesHandler = responsesHandler
	}
	if len(cfg.OpenAISpeechModels) > 0 {
		var handler *openaiProtocol.SpeechHandler
		if audioChargeBilling == nil {
			handler = openaiProtocol.NewSpeechHandler(logger, apiKeyAuthenticator, audioModels, openaiProvider.NewSpeech(providerCredentialRegistry, cfg.SpeechTimeout, cfg.SpeechStreamIdleTimeout), healthGate, cfg.SpeechRequestBytes, cfg.SpeechResponseBytes)
		} else {
			handler = openaiProtocol.NewBillableSpeechHandler(logger, apiKeyAuthenticator, audioModels, openaiProvider.NewSpeech(providerCredentialRegistry, cfg.SpeechTimeout, cfg.SpeechStreamIdleTimeout), healthGate, cfg.SpeechRequestBytes, cfg.SpeechResponseBytes, audioChargeBilling)
		}
		handler.SetTelemetry(telemetryRuntime.Recorder)
		handler.SetManagedOutputs(speechOutputService)
		openAISpeechHandler = handler
	}
	if len(cfg.OpenAITranscriptionModels) > 0 {
		var handler *openaiProtocol.TranscriptionHandler
		if transcriptionChargeBilling == nil {
			handler = openaiProtocol.NewTranscriptionHandler(logger, apiKeyAuthenticator, transcriptionModels, openaiProvider.NewTranscription(providerCredentialRegistry, cfg.TranscriptionTimeout, cfg.TranscriptionStreamIdleTimeout), healthGate, cfg.TranscriptionRequestBytes, cfg.TranscriptionFileBytes, cfg.TranscriptionFieldBytes, cfg.TranscriptionResponseBytes, cfg.TranscriptionSpoolLimit)
		} else {
			handler = openaiProtocol.NewBillableTranscriptionHandler(logger, apiKeyAuthenticator, transcriptionModels, openaiProvider.NewTranscription(providerCredentialRegistry, cfg.TranscriptionTimeout, cfg.TranscriptionStreamIdleTimeout), healthGate, cfg.TranscriptionRequestBytes, cfg.TranscriptionFileBytes, cfg.TranscriptionFieldBytes, cfg.TranscriptionResponseBytes, cfg.TranscriptionSpoolLimit, transcriptionChargeBilling)
		}
		handler.SetAudioAssets(audioAssetService)
		handler.SetTelemetry(telemetryRuntime.Recorder)
		openAITranscriptionHandler = handler
	}
	if len(cfg.OpenAITranslationModels) > 0 {
		var handler *openaiProtocol.TranslationHandler
		if translationChargeBilling == nil {
			handler = openaiProtocol.NewTranslationHandler(logger, apiKeyAuthenticator, translationModels, openaiProvider.NewTranslation(providerCredentialRegistry, cfg.TranslationTimeout), healthGate, cfg.TranslationRequestBytes, cfg.TranslationFileBytes, cfg.TranslationFieldBytes, cfg.TranslationResponseBytes, cfg.TranslationSpoolLimit)
		} else {
			handler = openaiProtocol.NewBillableTranslationHandler(logger, apiKeyAuthenticator, translationModels, openaiProvider.NewTranslation(providerCredentialRegistry, cfg.TranslationTimeout), healthGate, cfg.TranslationRequestBytes, cfg.TranslationFileBytes, cfg.TranslationFieldBytes, cfg.TranslationResponseBytes, cfg.TranslationSpoolLimit, translationChargeBilling)
		}
		handler.SetAudioAssets(audioAssetService)
		handler.SetTelemetry(telemetryRuntime.Recorder)
		openAITranslationHandler = handler
	}
	if len(cfg.AnthropicMessagesModels) > 0 {
		var handler *anthropicProtocol.Handler
		if anthropicChargeBilling == nil {
			handler = anthropicProtocol.NewHandler(logger, apiKeyAuthenticator, anthropicModels, anthropicProvider.New(providerCredentialRegistry, cfg.AnthropicTimeout, cfg.AnthropicStreamIdleTimeout), providerCredentialRegistry, healthGate, cfg.AnthropicBodyBytes, false)
		} else {
			handler = anthropicProtocol.NewBillableHandler(logger, apiKeyAuthenticator, anthropicModels, anthropicProvider.New(providerCredentialRegistry, cfg.AnthropicTimeout, cfg.AnthropicStreamIdleTimeout), providerCredentialRegistry, healthGate, cfg.AnthropicBodyBytes, anthropicChargeBilling)
		}
		handler.SetTelemetry(telemetryRuntime.Recorder)
		anthropicHandler = handler
	}
	var geminiHandler *gemini.Handler
	var openAIImagesHandler *openaiProtocol.Handler
	var openAIImageEditsHandler *openaiProtocol.EditHandler
	if chargeBilling == nil {
		geminiHandler = gemini.NewHandlerWithImageAndLLMModels(logger, apiKeyAuthenticator, imageModels, geminiExecutor, cfg.GeminiBodyBytes, providerAvailability, healthGate, geminiLLMModels)
		openAIImagesHandler = openaiProtocol.NewImagesHandlerWithHealth(logger, apiKeyAuthenticator, imageModels, imageExecutors, cfg.ImagesBodyBytes, healthGate)
		openAIImageEditsHandler = openaiProtocol.NewEditHandlerWithHealth(logger, apiKeyAuthenticator, imageModels, imageExecutors, cfg.ImageEditsBodyBytes, cfg.ImageEditSpoolLimit, healthGate)
	} else {
		geminiHandler = gemini.NewBillableHandlerWithLLMTokenBilling(logger, apiKeyAuthenticator, imageModels, geminiExecutor, cfg.GeminiBodyBytes, chargeBilling, chatChargeBilling, providerAvailability, healthGate, geminiLLMModels)
		openAIImagesHandler = openaiProtocol.NewBillableImagesHandlerWithAvailabilityAndHealth(logger, apiKeyAuthenticator, imageModels, imageExecutors, cfg.ImagesBodyBytes, chargeBilling, providerAvailability, healthGate)
		openAIImageEditsHandler = openaiProtocol.NewBillableEditHandlerWithAvailabilityAndHealth(logger, apiKeyAuthenticator, imageModels, imageExecutors, cfg.ImageEditsBodyBytes, cfg.ImageEditSpoolLimit, chargeBilling, providerAvailability, healthGate)
	}
	if imageResults != nil {
		geminiHandler.SetResultManager(imageResults)
		openAIImagesHandler.SetResultManager(imageResults)
		openAIImageEditsHandler.SetResultManager(imageResults)
	}
	if reconciliationWorker != nil {
		reconciliationWorker.SetTelemetry(telemetryRuntime.Recorder)
	}
	geminiHandler.SetTelemetry(telemetryRuntime.Recorder)
	openAIImagesHandler.SetTelemetry(telemetryRuntime.Recorder)
	openAIImageEditsHandler.SetTelemetry(telemetryRuntime.Recorder)
	openAIModelsHandler := openaiProtocol.NewModelsHandlerWithAllAudioOperations(logger, apiKeyAuthenticator, imageModels, chatModels, responsesModels, videoModels, audioModels, transcriptionModels, translationModels, providerAvailability)
	openAIModelsHandler.SetTranscriptionPricing(transcriptionPricing)
	openAIModelsHandler.SetTranslationPricing(translationPricing)
	var replicateHandler http.Handler
	var replicateWebhookHandler http.Handler
	var falHandler http.Handler
	var falWebhookHandler http.Handler
	var pluginWebhookHandler http.Handler
	var pluginVideoWebhookHandler http.Handler
	var managementHandler http.Handler
	var runwayHandler http.Handler
	var asyncWorker *jobs.Worker
	asyncProviders := map[string]jobs.Provider{}
	if pluginClient != nil && (pluginRegistry.SupportsAsyncProtocol("replicate") || pluginRegistry.SupportsAsyncProtocol("fal") || pluginRegistry.SupportsVideo()) {
		pluginAsync, pluginErr := pluginProvider.NewAsync(pluginClient, cfg.ReconcileInterval)
		if pluginErr != nil {
			logger.Error("gateway async plugin provider initialization failed")
			return 1
		}
		asyncProviders["plugin"] = pluginAsync
	}
	var replicateAdapter *replicateProvider.Client
	var falAdapter *falProvider.Client
	var runwayAdapter *runwayProvider.Client
	var runwayAssetStore *runwayassets.Store
	if cfg.ReplicateEnabled {
		var replicateErr error
		replicateAdapter, replicateErr = replicateProvider.New(replicateProvider.Config{Endpoint: cfg.ReplicateEndpoint, PublicBaseURL: cfg.PublicBaseURL, Timeout: cfg.ReplicateTimeout, MaximumBodyBytes: cfg.ReplicateBodyBytes}, providerCredentialRegistry)
		if replicateErr != nil {
			logger.Error("gateway Replicate provider initialization failed")
			return 1
		}
		asyncProviders["replicate"] = replicateAdapter
	}
	if cfg.FalEnabled {
		var falErr error
		falAdapter, falErr = falProvider.New(falProvider.Config{Endpoint: cfg.FalEndpoint, PublicBaseURL: cfg.PublicBaseURL, Timeout: cfg.FalTimeout, MaximumBodyBytes: cfg.FalBodyBytes}, providerCredentialRegistry)
		if falErr != nil {
			logger.Error("gateway fal provider initialization failed")
			return 1
		}
		asyncProviders["fal"] = falAdapter
	}
	if cfg.RunwayEnabled {
		var runwayErr error
		runwayAdapter, runwayErr = runwayProvider.New(runwayProvider.Config{Endpoint: "https://api.dev.runwayml.com", Timeout: cfg.RunwayTimeout, MaximumBodyBytes: cfg.RunwayBodyBytes}, providerCredentialRegistry)
		if runwayErr != nil {
			logger.Error("gateway Runway provider initialization failed")
			return 1
		}
		runwayAssetStore, runwayErr = runwayassets.NewStore(pool)
		if runwayErr != nil {
			logger.Error("gateway Runway asset store initialization failed")
			return 1
		}
		asyncProviders["runway"] = runwayAdapter
		readinessChecks = append(readinessChecks, runwayAssetStore.Ready)
	}
	if runwayAssetStore == nil && pluginRegistry != nil && pluginRegistry.SupportsVideo() {
		var runwayErr error
		runwayAssetStore, runwayErr = runwayassets.NewStore(pool)
		if runwayErr != nil {
			logger.Error("gateway Runway asset store initialization failed")
			return 1
		}
		readinessChecks = append(readinessChecks, runwayAssetStore.Ready)
	}
	if len(asyncProviders) > 0 {
		jobRepository, repositoryErr := jobs.NewRepository(pool, cfg.ReplayBodyBytes)
		if repositoryErr != nil {
			logger.Error("gateway Job repository initialization failed")
			return 1
		}
		jobPollDelay := cfg.ReconcileInterval
		if cfg.RunwayEnabled && jobPollDelay < cfg.RunwayPollInterval {
			jobPollDelay = cfg.RunwayPollInterval
		}
		serviceConfig := jobs.ServiceConfig{SubmitLease: cfg.ReconcileLease, PollDelay: jobPollDelay}
		serviceConfig.Webhooks = map[string]jobs.WebhookConfig{}
		if cfg.ReplicateWebhookMode == config.ReplicateWebhookRequired {
			serviceConfig.Webhooks["replicate"] = jobs.WebhookConfig{PublicBaseURL: cfg.PublicBaseURL, BindingTTL: cfg.ReplicateWebhookBindingTTL, CallbackSecret: cfg.ReplicateWebhookCallbackSecret}
		}
		if cfg.FalWebhookMode == config.FalWebhookRequired {
			serviceConfig.Webhooks["fal"] = jobs.WebhookConfig{PublicBaseURL: cfg.PublicBaseURL, BindingTTL: cfg.FalWebhookBindingTTL, CallbackSecret: cfg.FalWebhookCallbackSecret}
		}
		if pluginRegistry != nil && pluginRegistry.SupportsCallbacks() {
			if len(cfg.PluginCallbackSecrets) == 0 {
				logger.Error("gateway plugin callback secret initialization failed")
				return 1
			}
			serviceConfig.Webhooks["plugin"] = jobs.WebhookConfig{PublicBaseURL: cfg.PublicBaseURL, BindingTTL: cfg.PluginCallbackBindingTTL, CallbackSecret: cfg.PluginCallbackSecrets[0], EnabledChannel: func(channelID string) bool {
				binding, ok := pluginRegistry.Binding(channelID)
				return ok && binding.Async && binding.Callback
			}, PathPrefix: func(channelID string) string {
				binding, ok := pluginRegistry.Binding(channelID)
				if ok && binding.Video {
					return "/internal/webhooks/plugin-video"
				}
				return "/internal/webhooks/plugin"
			}}
		}
		jobService, serviceErr := jobs.NewService(jobRepository, asyncProviders, serviceConfig, "gateway-submit")
		if serviceErr != nil {
			logger.Error("gateway Job service initialization failed")
			return 1
		}
		jobService.SetTelemetry(telemetryRuntime.Recorder)
		if pluginRegistry != nil && pluginRegistry.SupportsCallbacks() {
			pluginWebhookHandler, serviceErr = pluginProvider.NewCallbackHandler(pluginRegistry, pluginProvider.ServiceCallbackApplier{Service: jobService}, cfg.PluginCallbackSecrets, cfg.PluginCallbackTolerance, cfg.PluginCallbackBodyBytes)
			if serviceErr != nil {
				logger.Error("gateway plugin callback handler initialization failed")
				return 1
			}
			if pluginRegistry.SupportsVideo() {
				pluginVideoWebhookHandler, serviceErr = pluginProvider.NewVideoCallbackHandler(pluginRegistry, pluginProvider.ServiceCallbackApplier{Service: jobService}, cfg.PluginCallbackSecrets, cfg.PluginCallbackTolerance, cfg.PluginCallbackBodyBytes)
				if serviceErr != nil {
					logger.Error("gateway plugin video callback handler initialization failed")
					return 1
				}
			}
		}
		if cfg.JobManagementMode == config.JobManagementRequired {
			managementHandler, serviceErr = managementProtocol.NewHandler(apiKeyAuthenticator, jobRepository, cfg.JobManagementCursorSecrets)
			if serviceErr != nil {
				logger.Error("gateway Job management initialization failed")
				return 1
			}
		}
		workerConfig := jobs.WorkerConfig{Interval: cfg.ReconcileInterval, Lease: cfg.ReconcileLease, PollDelay: jobPollDelay, BaseBackoff: cfg.ReconcileBackoff, MaximumBackoff: cfg.ReconcileMaxBackoff, BatchSize: cfg.ReconcileBatchSize, MaximumAttempts: cfg.ReconcileMaxAttempts}
		asyncWorker, serviceErr = jobs.NewWorker(jobRepository, asyncProviders, billingService, workerConfig, "gateway-job-worker")
		if serviceErr != nil {
			logger.Error("gateway Job worker initialization failed")
			return 1
		}
		if videoResults != nil || imageResults != nil {
			asyncWorker.SetResultManager(jobs.ResultRouter{Image: jobs.ImageResultManager{Manager: imageResults}, Video: videoResults})
		}
		asyncWorker.SetTelemetry(telemetryRuntime.Recorder)
		if cfg.ReplicateEnabled || pluginRegistry != nil && pluginRegistry.SupportsAsyncProtocol("replicate") {
			replicateHandler = replicateProtocol.NewHandler(logger, apiKeyAuthenticator, imageModels, jobService, billingService, providerCredentialRegistry, cfg.ReplicateBodyBytes, cfg.PublicBaseURL)
			replicateHandler.(*replicateProtocol.Handler).SetManagedResults(imageResults != nil)
			if cfg.ReplicateWebhookMode == config.ReplicateWebhookRequired {
				verifier, verifierErr := replicateProtocol.NewSignatureVerifier(cfg.ReplicateWebhookSecrets, cfg.ReplicateWebhookTolerance)
				if verifierErr != nil {
					logger.Error("gateway Replicate webhook verifier initialization failed")
					return 1
				}
				replicateWebhookHandler, serviceErr = replicateProtocol.NewWebhookHandler(logger, verifier, jobService, replicateAdapter, cfg.ReplicateBodyBytes)
				if serviceErr != nil {
					logger.Error("gateway Replicate webhook handler initialization failed")
					return 1
				}
				replicateWebhookHandler.(*replicateProtocol.WebhookHandler).SetTelemetry(telemetryRuntime.Recorder)
			}
		}
		if cfg.FalEnabled || pluginRegistry != nil && pluginRegistry.SupportsAsyncProtocol("fal") {
			falHandler = falProtocol.NewHandler(logger, apiKeyAuthenticator, imageModels, jobService, billingService, providerCredentialRegistry, cfg.FalBodyBytes, cfg.PublicBaseURL)
			falHandler.(*falProtocol.Handler).SetManagedResults(imageResults != nil)
			if cfg.FalWebhookMode == config.FalWebhookRequired {
				verifier, verifierErr := falProtocol.NewFalJWKSVerifier(falProtocol.JWKSConfig{URL: cfg.FalJWKSURL, ExpectedURL: "https://rest.fal.ai/.well-known/jwks.json", Timeout: cfg.FalJWKSTimeout, CacheTTL: cfg.FalJWKSCacheTTL, RefreshCooldown: cfg.FalJWKSRefreshCooldown, MaximumBodyBytes: 64 * 1024})
				if verifierErr != nil {
					logger.Error("gateway fal webhook verifier initialization failed")
					return 1
				}
				webhook, webhookErr := falProtocol.NewWebhookHandler(logger, verifier, jobService, falAdapter, cfg.FalBodyBytes)
				if webhookErr != nil {
					logger.Error("gateway fal webhook handler initialization failed")
					return 1
				}
				webhook.SetTelemetry(telemetryRuntime.Recorder)
				falWebhookHandler = webhook
			}
		}
		if cfg.RunwayEnabled || pluginRegistry != nil && pluginRegistry.SupportsVideo() {
			handler := runwayProtocol.NewHandler(logger, apiKeyAuthenticator, videoModels, jobService, cfg.RunwayBodyBytes)
			if runwayAdapter != nil {
				handler.SetUploads(runwayAdapter, runwayAssetStore)
			} else {
				handler.SetUploads(nil, runwayAssetStore)
			}
			handler.SetManagedResults(videoResults != nil)
			if cfg.BillingMode == config.BillingRequired {
				handler.SetBilling(billingService)
			}
			runwayHandler = handler
		}
		readinessChecks = append(readinessChecks, jobRepository.Ready)
	}
	clientIPResolver, resolverErr := clientip.New(cfg.TrustedProxyPrefixes)
	if resolverErr != nil {
		logger.Error("gateway client IP resolver initialization failed")
		return 1
	}

	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	workerCount := 0
	workerFinished := make(chan struct{}, 7)
	if speechOutputService != nil {
		workerCount++
		go func() {
			defer func() { workerFinished <- struct{}{} }()
			ticker := time.NewTicker(cfg.SpeechOutputStorage.CleanupInterval)
			defer ticker.Stop()
			for {
				if _, workerErr := speechOutputService.RunCleanup(workerCtx); workerErr != nil {
					logger.Warn("Speech output cleanup cycle failed", "category", "speech_output_cleanup_failed")
				}
				select {
				case <-workerCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	if audioAssetService != nil {
		workerCount++
		go func() {
			defer func() { workerFinished <- struct{}{} }()
			ticker := time.NewTicker(cfg.AudioInputStorage.CleanupInterval)
			defer ticker.Stop()
			for {
				if _, workerErr := audioAssetService.RunCleanup(workerCtx); workerErr != nil {
					logger.Warn("Audio asset cleanup cycle failed", "category", "audio_asset_cleanup_failed")
				}
				select {
				case <-workerCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	if reconciliationWorker != nil {
		workerCount++
		go func() {
			defer func() { workerFinished <- struct{}{} }()
			reconciliationWorker.Run(workerCtx, func(err error) {
				logger.Warn("reconciliation cycle failed", "category", "worker_cycle_failed")
			})
		}()
	}
	if chatReconciliationWorker != nil {
		workerCount++
		go func() {
			defer func() { workerFinished <- struct{}{} }()
			ticker := time.NewTicker(cfg.ReconcileInterval)
			defer ticker.Stop()
			for {
				if _, workerErr := chatReconciliationWorker.RunOne(workerCtx); workerErr != nil {
					logger.Warn("Chat reconciliation cycle failed", "category", "chat_reconciliation_failed")
				}
				select {
				case <-workerCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	if transcriptionReconciliationWorker != nil {
		workerCount++
		go func() {
			defer func() { workerFinished <- struct{}{} }()
			ticker := time.NewTicker(cfg.ReconcileInterval)
			defer ticker.Stop()
			for {
				if _, workerErr := transcriptionReconciliationWorker.RunOne(workerCtx); workerErr != nil {
					logger.Warn("Audio Transcription reconciliation cycle failed", "category", "transcription_reconciliation_failed")
				}
				select {
				case <-workerCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	if translationReconciliationWorker != nil {
		workerCount++
		go func() {
			defer func() { workerFinished <- struct{}{} }()
			ticker := time.NewTicker(cfg.ReconcileInterval)
			defer ticker.Stop()
			for {
				if _, workerErr := translationReconciliationWorker.RunOne(workerCtx); workerErr != nil {
					logger.Warn("Audio Translation reconciliation cycle failed", "category", "translation_reconciliation_failed")
				}
				select {
				case <-workerCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	if asyncWorker != nil {
		workerCount++
		go func() {
			defer func() { workerFinished <- struct{}{} }()
			asyncWorker.Run(workerCtx, func(error) { logger.Warn("asynchronous Job cycle failed", "category", "worker_cycle_failed") })
		}()
	}
	if workerCount == 0 {
		close(workerDone)
	} else {
		go func() {
			for range workerCount {
				<-workerFinished
			}
			close(workerDone)
		}()
	}
	err = app.Run(ctx, cfg, logger, app.Dependencies{
		Ready:                ready,
		ProviderCredentials:  providerCredentialRegistry,
		Gemini:               geminiHandler,
		OpenAIImages:         openAIImagesHandler,
		OpenAIImageEdits:     openAIImageEditsHandler,
		OpenAIModels:         openAIModelsHandler,
		OpenAIChat:           openAIChatHandler,
		OpenAIResponses:      openAIResponsesHandler,
		OpenAISpeech:         openAISpeechHandler,
		OpenAISpeechAssets:   openAISpeechAssetHandler,
		OpenAITranscriptions: openAITranscriptionHandler,
		OpenAITranslations:   openAITranslationHandler,
		OpenAIAudioAssets:    openAIAudioAssetHandler,
		Anthropic:            anthropicHandler,
		Replicate:            replicateHandler,
		ReplicateWebhook:     replicateWebhookHandler,
		Fal:                  falHandler,
		FalWebhook:           falWebhookHandler,
		PluginWebhook:        pluginWebhookHandler,
		PluginVideoWebhook:   pluginVideoWebhookHandler,
		Runway:               runwayHandler,
		Management:           managementHandler,
		ClientIPResolver:     clientIPResolver,
		Telemetry:            telemetryRuntime.Recorder,
		TracePropagator:      telemetryRuntime.Propagator,
	})
	cancelWorker()
	<-workerDone
	if err != nil {
		logger.Error("gateway stopped with error", "error", err.Error())
		return 1
	}
	return 0
}
