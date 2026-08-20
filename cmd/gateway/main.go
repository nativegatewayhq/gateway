package main

import (
	"context"
	"fmt"
	"io"
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
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/networkauth"
	"github.com/nativegatewayhq/gateway/internal/observability"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/ratelimit"
	"github.com/nativegatewayhq/gateway/internal/reconciliation"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	"github.com/nativegatewayhq/gateway/protocols/gemini"
	openaiProtocol "github.com/nativegatewayhq/gateway/protocols/openai"
	"github.com/nativegatewayhq/gateway/providers/google"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
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
	imageModels := imageoperation.DefaultRegistry()
	openAIExecutor := openaiProvider.New(providerCredentialRegistry, cfg.ImagesTimeout)
	xAIExecutor := xai.New(providerCredentialRegistry, cfg.ImagesTimeout)
	imageExecutors := map[providercredentials.ProviderID]openaiProtocol.Executor{
		providercredentials.OpenAI: openAIExecutor,
		providercredentials.XAI:    xAIExecutor,
	}
	var chargeBilling openaiProtocol.Billing
	var reconciliationWorker *reconciliation.Worker
	if cfg.BillingMode == config.BillingRequired {
		priceEstimator, pricingErr := pricing.NewService(pool, cfg.MinimumMarginBPS)
		if pricingErr != nil {
			logger.Error("gateway pricing initialization failed")
			return 1
		}
		billingService, billingErr := chargebilling.NewServiceWithControls(pool, priceEstimator, ledger.NewService(pool), costquota.NewStore(pool), spendcap.NewStore(pool), cfg.ReplayBodyBytes)
		if billingErr != nil {
			logger.Error("gateway billing initialization failed")
			return 1
		}
		chargeBilling = billingService
		reconciliationWorker, billingErr = reconciliation.New(pool, billingService, reconciliation.Config{
			Interval: cfg.ReconcileInterval, Lease: cfg.ReconcileLease,
			BaseBackoff: cfg.ReconcileBackoff, MaxBackoff: cfg.ReconcileMaxBackoff,
			BatchSize: cfg.ReconcileBatchSize, MaxAttempts: cfg.ReconcileMaxAttempts,
		})
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
	openAIModelsHandler := openaiProtocol.NewModelsHandler(logger, apiKeyAuthenticator, imageModels, providerCredentialRegistry)
	clientIPResolver, resolverErr := clientip.New(cfg.TrustedProxyPrefixes)
	if resolverErr != nil {
		logger.Error("gateway client IP resolver initialization failed")
		return 1
	}

	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	if reconciliationWorker != nil {
		go func() {
			defer close(workerDone)
			reconciliationWorker.Run(workerCtx, func(err error) {
				logger.Warn("reconciliation cycle failed", "category", "worker_cycle_failed")
			})
		}()
	} else {
		close(workerDone)
	}
	err = app.Run(ctx, cfg, logger, app.Dependencies{
		Ready:               ready,
		ProviderCredentials: providerCredentialRegistry,
		Gemini:              geminiHandler,
		OpenAIImages:        openAIImagesHandler,
		OpenAIImageEdits:    openAIImageEditsHandler,
		OpenAIModels:        openAIModelsHandler,
		ClientIPResolver:    clientIPResolver,
	})
	cancelWorker()
	<-workerDone
	if err != nil {
		logger.Error("gateway stopped with error", "error", err.Error())
		return 1
	}
	return 0
}
