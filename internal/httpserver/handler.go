// Package httpserver defines the Gateway-owned HTTP surface and middleware.
package httpserver

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/nativegatewayhq/gateway/internal/clientip"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	"go.opentelemetry.io/otel/propagation"
)

// ReadyFunc checks dependencies required to accept traffic. A nil check means
// the process has no external readiness dependencies yet.
type ReadyFunc func(context.Context) error

type Routes struct {
	Gemini               http.Handler
	OpenAIImages         http.Handler
	OpenAIImageEdits     http.Handler
	OpenAIModels         http.Handler
	OpenAIChat           http.Handler
	OpenAIResponses      http.Handler
	OpenAISpeech         http.Handler
	OpenAISpeechAssets   http.Handler
	OpenAITranscriptions http.Handler
	OpenAITranslations   http.Handler
	OpenAIAudioAssets    http.Handler
	Anthropic            http.Handler
	Replicate            http.Handler
	ReplicateWebhook     http.Handler
	Fal                  http.Handler
	FalWebhook           http.Handler
	PluginWebhook        http.Handler
	Runway               http.Handler
	Management           http.Handler
}

// NewHandler builds the Gateway-owned and accepted provider-native routes.
func NewHandler(logger *slog.Logger, ready ReadyFunc, routeSets ...Routes) http.Handler {
	resolver, _ := clientip.New(nil)
	return NewHandlerWithClientIP(logger, ready, resolver, routeSets...)
}

// NewHandlerWithClientIP resolves the client address before protocol authentication.
func NewHandlerWithClientIP(logger *slog.Logger, ready ReadyFunc, resolver *clientip.Resolver, routeSets ...Routes) http.Handler {
	return NewHandlerWithTelemetry(logger, ready, resolver, nil, nil, routeSets...)
}

func NewHandlerWithTelemetry(logger *slog.Logger, ready ReadyFunc, resolver *clientip.Resolver, recorder *telemetry.Recorder, propagator propagation.TextMapPropagator, routeSets ...Routes) http.Handler {
	mux := http.NewServeMux()
	if len(routeSets) > 0 && routeSets[0].Gemini != nil {
		mux.Handle("/v1beta/models/", routeSets[0].Gemini)
	}
	if len(routeSets) > 0 && routeSets[0].OpenAIImages != nil {
		mux.Handle("/v1/images/generations", routeSets[0].OpenAIImages)
	}
	if len(routeSets) > 0 && routeSets[0].OpenAIImageEdits != nil {
		mux.Handle("/v1/images/edits", routeSets[0].OpenAIImageEdits)
	}
	if len(routeSets) > 0 && routeSets[0].OpenAIModels != nil {
		mux.Handle("/v1/models", routeSets[0].OpenAIModels)
	}
	if len(routeSets) > 0 && routeSets[0].OpenAIChat != nil {
		mux.Handle("/v1/chat/completions", routeSets[0].OpenAIChat)
	}
	if len(routeSets) > 0 && routeSets[0].OpenAIResponses != nil {
		mux.Handle("/v1/responses", routeSets[0].OpenAIResponses)
	}
	if len(routeSets) > 0 && routeSets[0].OpenAISpeech != nil {
		mux.Handle("/v1/audio/speech", routeSets[0].OpenAISpeech)
	}
	if len(routeSets) > 0 && routeSets[0].OpenAISpeechAssets != nil {
		mux.Handle("/v1/audio/speech/assets/", routeSets[0].OpenAISpeechAssets)
	}
	if len(routeSets) > 0 && routeSets[0].OpenAITranscriptions != nil {
		mux.Handle("/v1/audio/transcriptions", routeSets[0].OpenAITranscriptions)
	}
	if len(routeSets) > 0 && routeSets[0].OpenAITranslations != nil {
		mux.Handle("/v1/audio/translations", routeSets[0].OpenAITranslations)
	}
	if len(routeSets) > 0 && routeSets[0].OpenAIAudioAssets != nil {
		mux.Handle("/v1/audio/assets", routeSets[0].OpenAIAudioAssets)
		mux.Handle("/v1/audio/assets/", routeSets[0].OpenAIAudioAssets)
	}
	if len(routeSets) > 0 && routeSets[0].Anthropic != nil {
		mux.Handle("/v1/messages", routeSets[0].Anthropic)
	}
	if len(routeSets) > 0 && routeSets[0].Runway != nil {
		mux.Handle("/v1/text_to_video", routeSets[0].Runway)
		mux.Handle("/v1/image_to_video", routeSets[0].Runway)
		mux.Handle("/v1/tasks/", routeSets[0].Runway)
	}
	mux.HandleFunc("/v1/chat/", func(writer http.ResponseWriter, request *http.Request) {
		writeError(writer, request, http.StatusNotFound, "not_found", "route not found")
	})
	if len(routeSets) > 0 && routeSets[0].Replicate != nil {
		mux.Handle("/v1/predictions", routeSets[0].Replicate)
		mux.Handle("/v1/predictions/", routeSets[0].Replicate)
	}
	if len(routeSets) > 0 && routeSets[0].ReplicateWebhook != nil {
		mux.Handle("/internal/webhooks/replicate/", routeSets[0].ReplicateWebhook)
	}
	if len(routeSets) > 0 && routeSets[0].FalWebhook != nil {
		mux.Handle("/internal/webhooks/fal/", routeSets[0].FalWebhook)
	}
	if len(routeSets) > 0 && routeSets[0].PluginWebhook != nil {
		mux.Handle("/internal/webhooks/plugin/", routeSets[0].PluginWebhook)
	}
	if len(routeSets) > 0 && routeSets[0].Management != nil {
		mux.Handle("/gateway/v1/jobs", routeSets[0].Management)
		mux.Handle("/gateway/v1/jobs/", routeSets[0].Management)
	}
	mux.HandleFunc("GET /health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /health/ready", func(writer http.ResponseWriter, request *http.Request) {
		if ready != nil && ready(request.Context()) != nil {
			writeError(writer, request, http.StatusServiceUnavailable, "not_ready", "service is not ready")
			return
		}
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	if len(routeSets) > 0 && routeSets[0].Fal != nil {
		mux.Handle("/", routeSets[0].Fal)
	} else {
		mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
			writeError(writer, request, http.StatusNotFound, "not_found", "route not found")
		})
	}

	handler := recovery(logger, mux)
	handler = accessLog(logger, handler)
	handler = resolver.Middleware(handler)
	if recorder != nil {
		handler = recorder.Middleware(propagator, handler)
	}
	handler = requestid.Middleware(handler)
	return handler
}
