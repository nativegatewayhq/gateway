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
	Gemini           http.Handler
	OpenAIImages     http.Handler
	OpenAIImageEdits http.Handler
	OpenAIModels     http.Handler
	Replicate        http.Handler
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
	if len(routeSets) > 0 && routeSets[0].Replicate != nil {
		mux.Handle("/v1/predictions", routeSets[0].Replicate)
		mux.Handle("/v1/predictions/", routeSets[0].Replicate)
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
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writeError(writer, request, http.StatusNotFound, "not_found", "route not found")
	})

	handler := recovery(logger, mux)
	handler = accessLog(logger, handler)
	handler = resolver.Middleware(handler)
	if recorder != nil {
		handler = recorder.Middleware(propagator, handler)
	}
	handler = requestid.Middleware(handler)
	return handler
}
