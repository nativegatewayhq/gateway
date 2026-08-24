// Package app composes and runs the Gateway process.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/nativegatewayhq/gateway/internal/clientip"
	"github.com/nativegatewayhq/gateway/internal/config"
	"github.com/nativegatewayhq/gateway/internal/httpserver"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	"go.opentelemetry.io/otel/propagation"
)

type Dependencies struct {
	Ready                httpserver.ReadyFunc
	ProviderCredentials  *providercredentials.Registry
	Gemini               http.Handler
	OpenAIImages         http.Handler
	OpenAIImageEdits     http.Handler
	OpenAIModels         http.Handler
	OpenAIChat           http.Handler
	OpenAIResponses      http.Handler
	OpenAISpeech         http.Handler
	OpenAITranscriptions http.Handler
	OpenAITranslations   http.Handler
	Anthropic            http.Handler
	Replicate            http.Handler
	ReplicateWebhook     http.Handler
	Fal                  http.Handler
	FalWebhook           http.Handler
	Runway               http.Handler
	Management           http.Handler
	ClientIPResolver     *clientip.Resolver
	Telemetry            *telemetry.Recorder
	TracePropagator      propagation.TextMapPropagator
}

// Error reports a lifecycle failure category without exposing listener or
// configuration values. The wrapped cause remains available to internal
// callers through errors.Is and errors.As.
type Error struct {
	Kind string
	err  error
}

func (err *Error) Error() string { return err.Kind }

func (err *Error) Unwrap() error { return err.err }

func runtimeError(kind string, err error) error {
	return &Error{Kind: kind, err: err}
}

// Run starts the HTTP server and blocks until it fails or ctx is canceled.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, dependencies Dependencies) error {
	if dependencies.ProviderCredentials == nil {
		return runtimeError("provider credential registry unavailable", nil)
	}
	if dependencies.Gemini == nil {
		return runtimeError("gemini handler unavailable", nil)
	}
	if dependencies.OpenAIImages == nil {
		return runtimeError("openai images handler unavailable", nil)
	}
	if dependencies.OpenAIImageEdits == nil {
		return runtimeError("openai image edits handler unavailable", nil)
	}
	if dependencies.OpenAIModels == nil {
		return runtimeError("openai models handler unavailable", nil)
	}
	if dependencies.ClientIPResolver == nil {
		dependencies.ClientIPResolver, _ = clientip.New(nil)
	}
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return runtimeError("listen failed", err)
	}

	server := &http.Server{
		Handler:           httpserver.NewHandlerWithTelemetry(logger, dependencies.Ready, dependencies.ClientIPResolver, dependencies.Telemetry, dependencies.TracePropagator, httpserver.Routes{Gemini: dependencies.Gemini, OpenAIImages: dependencies.OpenAIImages, OpenAIImageEdits: dependencies.OpenAIImageEdits, OpenAIModels: dependencies.OpenAIModels, OpenAIChat: dependencies.OpenAIChat, OpenAIResponses: dependencies.OpenAIResponses, OpenAISpeech: dependencies.OpenAISpeech, OpenAITranscriptions: dependencies.OpenAITranscriptions, OpenAITranslations: dependencies.OpenAITranslations, Anthropic: dependencies.Anthropic, Replicate: dependencies.Replicate, ReplicateWebhook: dependencies.ReplicateWebhook, Fal: dependencies.Fal, FalWebhook: dependencies.FalWebhook, Runway: dependencies.Runway, Management: dependencies.Management}),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()

	logger.Info("gateway started", "address", listener.Addr().String())

	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return runtimeError("serve failed", err)
	case <-ctx.Done():
	}

	logger.Info("gateway shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return runtimeError("graceful shutdown timed out", err)
	}
	if err := <-serveErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return runtimeError("serve failed during shutdown", err)
	}

	logger.Info("gateway stopped")
	return nil
}
