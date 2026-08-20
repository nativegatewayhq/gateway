package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/app"
	chargebilling "github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/clientip"
	"github.com/nativegatewayhq/gateway/internal/config"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/imagestorage"
	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/networkauth"
	"github.com/nativegatewayhq/gateway/internal/observability"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/ratelimit"
	"github.com/nativegatewayhq/gateway/internal/reconciliation"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	"github.com/nativegatewayhq/gateway/protocols/gemini"
	openaiProtocol "github.com/nativegatewayhq/gateway/protocols/openai"
	replicateProtocol "github.com/nativegatewayhq/gateway/protocols/replicate"
	"github.com/nativegatewayhq/gateway/providers/google"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
	replicateProvider "github.com/nativegatewayhq/gateway/providers/replicate"
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
	googleExecutor := google.New(providerCredentialRegistry, cfg.GoogleTimeout)
	imageModels, err := imageoperation.DefaultRegistryWithReplicate(cfg.ReplicateModels)
	if err != nil {
		logger.Error("gateway model registry initialization failed")
		return 1
	}
	openAIExecutor := openaiProvider.New(providerCredentialRegistry, cfg.ImagesTimeout)
	xAIExecutor := xai.New(providerCredentialRegistry, cfg.ImagesTimeout)
	imageExecutors := map[providercredentials.ProviderID]openaiProtocol.Executor{
		providercredentials.OpenAI: openAIExecutor,
		providercredentials.XAI:    xAIExecutor,
	}
	var chargeBilling openaiProtocol.Billing
	var billingService *chargebilling.Service
	var reconciliationWorker *reconciliation.Worker
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
	var geminiHandler *gemini.Handler
	var openAIImagesHandler *openaiProtocol.Handler
	var openAIImageEditsHandler *openaiProtocol.EditHandler
	if chargeBilling == nil {
		geminiHandler = gemini.NewHandlerWithHealth(logger, apiKeyAuthenticator, googleExecutor, cfg.GeminiBodyBytes, healthGate)
		openAIImagesHandler = openaiProtocol.NewImagesHandlerWithHealth(logger, apiKeyAuthenticator, imageModels, imageExecutors, cfg.ImagesBodyBytes, healthGate)
		openAIImageEditsHandler = openaiProtocol.NewEditHandlerWithHealth(logger, apiKeyAuthenticator, imageModels, imageExecutors, cfg.ImageEditsBodyBytes, cfg.ImageEditSpoolLimit, healthGate)
	} else {
		geminiHandler = gemini.NewBillableHandlerWithAvailabilityAndHealth(logger, apiKeyAuthenticator, imageModels, googleExecutor, cfg.GeminiBodyBytes, chargeBilling, providerCredentialRegistry, healthGate)
		openAIImagesHandler = openaiProtocol.NewBillableImagesHandlerWithAvailabilityAndHealth(logger, apiKeyAuthenticator, imageModels, imageExecutors, cfg.ImagesBodyBytes, chargeBilling, providerCredentialRegistry, healthGate)
		openAIImageEditsHandler = openaiProtocol.NewBillableEditHandlerWithAvailabilityAndHealth(logger, apiKeyAuthenticator, imageModels, imageExecutors, cfg.ImageEditsBodyBytes, cfg.ImageEditSpoolLimit, chargeBilling, providerCredentialRegistry, healthGate)
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
	openAIModelsHandler := openaiProtocol.NewModelsHandler(logger, apiKeyAuthenticator, imageModels, providerCredentialRegistry)
	var replicateHandler http.Handler
	var asyncWorker *jobs.Worker
	if cfg.ReplicateEnabled {
		replicateAdapter, replicateErr := replicateProvider.New(replicateProvider.Config{Endpoint: cfg.ReplicateEndpoint, PublicBaseURL: cfg.PublicBaseURL, Timeout: cfg.ReplicateTimeout, MaximumBodyBytes: cfg.ReplicateBodyBytes}, providerCredentialRegistry)
		if replicateErr != nil {
			logger.Error("gateway Replicate provider initialization failed")
			return 1
		}
		jobRepository, repositoryErr := jobs.NewRepository(pool, cfg.ReplayBodyBytes)
		if repositoryErr != nil {
			logger.Error("gateway Job repository initialization failed")
			return 1
		}
		jobService, serviceErr := jobs.NewService(jobRepository, map[string]jobs.Provider{"replicate": replicateAdapter}, jobs.ServiceConfig{SubmitLease: cfg.ReconcileLease, PollDelay: cfg.ReconcileInterval}, "gateway-submit")
		if serviceErr != nil {
			logger.Error("gateway Job service initialization failed")
			return 1
		}
		jobService.SetTelemetry(telemetryRuntime.Recorder)
		workerConfig := jobs.WorkerConfig{Interval: cfg.ReconcileInterval, Lease: cfg.ReconcileLease, PollDelay: cfg.ReconcileInterval, BaseBackoff: cfg.ReconcileBackoff, MaximumBackoff: cfg.ReconcileMaxBackoff, BatchSize: cfg.ReconcileBatchSize, MaximumAttempts: cfg.ReconcileMaxAttempts}
		asyncWorker, serviceErr = jobs.NewWorker(jobRepository, map[string]jobs.Provider{"replicate": replicateAdapter}, billingService, workerConfig, "gateway-job-worker")
		if serviceErr != nil {
			logger.Error("gateway Job worker initialization failed")
			return 1
		}
		asyncWorker.SetTelemetry(telemetryRuntime.Recorder)
		replicateHandler = replicateProtocol.NewHandler(logger, apiKeyAuthenticator, imageModels, jobService, billingService, providerCredentialRegistry, cfg.ReplicateBodyBytes, cfg.PublicBaseURL)
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
	workerFinished := make(chan struct{}, 2)
	if reconciliationWorker != nil {
		workerCount++
		go func() {
			defer func() { workerFinished <- struct{}{} }()
			reconciliationWorker.Run(workerCtx, func(err error) {
				logger.Warn("reconciliation cycle failed", "category", "worker_cycle_failed")
			})
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
		Ready:               ready,
		ProviderCredentials: providerCredentialRegistry,
		Gemini:              geminiHandler,
		OpenAIImages:        openAIImagesHandler,
		OpenAIImageEdits:    openAIImageEditsHandler,
		OpenAIModels:        openAIModelsHandler,
		Replicate:           replicateHandler,
		ClientIPResolver:    clientIPResolver,
		Telemetry:           telemetryRuntime.Recorder,
		TracePropagator:     telemetryRuntime.Propagator,
	})
	cancelWorker()
	<-workerDone
	if err != nil {
		logger.Error("gateway stopped with error", "error", err.Error())
		return 1
	}
	return 0
}
