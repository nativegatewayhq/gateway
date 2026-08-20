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
	"github.com/nativegatewayhq/gateway/internal/config"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/observability"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
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
	apiKeyAuthenticator := apikey.NewService(apikey.NewPostgresStore(pool))
	googleExecutor := google.New(providerCredentialRegistry, cfg.GoogleTimeout)
	geminiHandler := gemini.NewHandler(logger, apiKeyAuthenticator, googleExecutor, cfg.GeminiBodyBytes)
	imageModels := imageoperation.DefaultRegistry()
	openAIExecutor := openaiProvider.New(providerCredentialRegistry, cfg.ImagesTimeout)
	xAIExecutor := xai.New(providerCredentialRegistry, cfg.ImagesTimeout)
	openAIImagesHandler := openaiProtocol.NewImagesHandler(logger, apiKeyAuthenticator, imageModels, map[providercredentials.ProviderID]openaiProtocol.Executor{
		providercredentials.OpenAI: openAIExecutor,
		providercredentials.XAI:    xAIExecutor,
	}, cfg.ImagesBodyBytes)

	if err := app.Run(ctx, cfg, logger, app.Dependencies{
		Ready:               pool.Ping,
		ProviderCredentials: providerCredentialRegistry,
		Gemini:              geminiHandler,
		OpenAIImages:        openAIImagesHandler,
	}); err != nil {
		logger.Error("gateway stopped with error", "error", err.Error())
		return 1
	}
	return 0
}
