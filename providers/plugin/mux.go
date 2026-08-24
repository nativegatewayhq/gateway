package plugin

import (
	"context"
	"net/http"

	"github.com/nativegatewayhq/gateway/internal/plugins"
	"github.com/nativegatewayhq/gateway/providers/google"
)

type GeminiExecutor interface {
	GenerateContent(context.Context, google.GenerateContentRequest) (*http.Response, error)
}

type GeminiMux struct {
	registry         *plugins.Registry
	plugin, fallback GeminiExecutor
}

func NewGeminiMux(registry *plugins.Registry, plugin, fallback GeminiExecutor) *GeminiMux {
	return &GeminiMux{registry: registry, plugin: plugin, fallback: fallback}
}

func (mux *GeminiMux) GenerateContent(ctx context.Context, request google.GenerateContentRequest) (*http.Response, error) {
	if binding, ok := mux.registry.Binding(request.ChannelID); ok && binding.Protocol == "gemini" {
		return mux.plugin.GenerateContent(ctx, request)
	}
	return mux.fallback.GenerateContent(ctx, request)
}
