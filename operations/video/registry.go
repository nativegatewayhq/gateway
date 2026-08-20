// Package video defines immutable video generation routes.
package video

import (
	"errors"
	"sort"
	"strings"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

type Operation string

const Generate Operation = "video.generate"

var ErrModelNotFound = errors.New("video model not found")

type Route struct {
	Model, ProviderModel, ChannelID string
	Provider                        providercredentials.ProviderID
	TextToVideo, ImageToVideo       bool
}

func (registry *Registry) List() []Route {
	if registry == nil {
		return nil
	}
	result := make([]Route, 0, len(registry.routes))
	for _, route := range registry.routes {
		result = append(result, route)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Model < result[j].Model })
	return result
}

type Registry struct{ routes map[string]Route }

func NewRegistry(models []string) (*Registry, error) {
	return NewRegistryWithCapabilities(models, nil)
}

type ModelCapability struct {
	ProviderModel string `json:"provider_model"`
	TextToVideo   bool   `json:"text_to_video"`
	ImageToVideo  bool   `json:"image_to_video"`
}

func NewRegistryWithCapabilities(models []string, capabilities map[string]ModelCapability) (*Registry, error) {
	routes := make(map[string]Route, len(models))
	usedCapabilities := 0
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || len(model) > 200 || strings.ContainsAny(model, "\r\n") {
			return nil, ErrModelNotFound
		}
		if _, exists := routes[model]; exists {
			return nil, ErrModelNotFound
		}
		capability, configured := capabilities[model]
		if configured {
			usedCapabilities++
		}
		if !configured {
			capability = ModelCapability{ProviderModel: model, TextToVideo: true, ImageToVideo: true}
		}
		if capability.ProviderModel == "" {
			capability.ProviderModel = model
		}
		if strings.TrimSpace(capability.ProviderModel) != capability.ProviderModel || len(capability.ProviderModel) > 200 || (!capability.TextToVideo && !capability.ImageToVideo) {
			return nil, ErrModelNotFound
		}
		routes[model] = Route{Model: model, ProviderModel: capability.ProviderModel, Provider: providercredentials.Runway, ChannelID: "channel_00000000000000000000000000000007", TextToVideo: capability.TextToVideo, ImageToVideo: capability.ImageToVideo}
	}
	if len(capabilities) > 0 && usedCapabilities != len(capabilities) {
		return nil, ErrModelNotFound
	}
	return &Registry{routes: routes}, nil
}

func (registry *Registry) Resolve(model string) (Route, error) {
	if registry == nil {
		return Route{}, ErrModelNotFound
	}
	route, ok := registry.routes[model]
	if !ok {
		return Route{}, ErrModelNotFound
	}
	return route, nil
}
