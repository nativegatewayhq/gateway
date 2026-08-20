// Package image defines immutable image operation capabilities.
package image

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

type Operation string
type MediaType string

const (
	Generate  Operation = "image.generate"
	Edit      Operation = "image.edit"
	JSON      MediaType = "application/json"
	Multipart MediaType = "multipart/form-data"
)

var (
	ErrModelNotFound  = errors.New("image model not found")
	ErrUnsupported    = errors.New("image capability unsupported")
	ErrInvalidModel   = errors.New("invalid image model")
	ErrDuplicateModel = errors.New("duplicate image model")
)

type Capability struct {
	Operation Operation
	MediaType MediaType
}
type ModelRoute struct {
	Protocol     string
	Model        string
	Provider     providercredentials.ProviderID
	ChannelID    string
	Owner        string
	Created      int64
	Capabilities []Capability
}
type Registry struct{ routes map[string]ModelRoute }

func NewRegistry(routes ...ModelRoute) (*Registry, error) {
	registry := &Registry{routes: make(map[string]ModelRoute, len(routes))}
	for _, route := range routes {
		if !validRouteProtocol(route.Protocol, route.Provider) || !validModelID(route.Model) || !validChannelID(route.ChannelID) || strings.TrimSpace(route.Owner) == "" || route.Created < 0 || len(route.Capabilities) == 0 {
			return nil, ErrInvalidModel
		}
		key := route.Protocol + "\x00" + route.Model
		if _, exists := registry.routes[key]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateModel, route.Model)
		}
		seen := make(map[Capability]struct{}, len(route.Capabilities))
		for _, capability := range route.Capabilities {
			if !validCapability(capability) {
				return nil, ErrInvalidModel
			}
			if _, exists := seen[capability]; exists {
				return nil, ErrInvalidModel
			}
			seen[capability] = struct{}{}
		}
		route.Capabilities = append([]Capability(nil), route.Capabilities...)
		registry.routes[key] = route
	}
	return registry, nil
}

func DefaultRegistry() *Registry {
	registry, err := NewRegistry(
		ModelRoute{Protocol: "openai", Model: "gpt-image-1", Provider: providercredentials.OpenAI, ChannelID: "channel_00000000000000000000000000000001", Owner: "openai", Capabilities: []Capability{{Generate, JSON}, {Edit, Multipart}}},
		ModelRoute{Protocol: "openai", Model: "grok-imagine-image-quality", Provider: providercredentials.XAI, ChannelID: "channel_00000000000000000000000000000002", Owner: "xai", Capabilities: []Capability{{Generate, JSON}, {Edit, JSON}}},
		ModelRoute{Protocol: "gemini", Model: "gemini-image", Provider: providercredentials.Google, ChannelID: "channel_00000000000000000000000000000003", Owner: "google", Capabilities: []Capability{{Generate, JSON}}},
	)
	if err != nil {
		panic("invalid built-in image model registry")
	}
	return registry
}

func (registry *Registry) Resolve(model string, operation Operation, mediaType MediaType) (ModelRoute, error) {
	return registry.ResolveProtocol("openai", model, operation, mediaType)
}

func (registry *Registry) ResolveProtocol(protocol, model string, operation Operation, mediaType MediaType) (ModelRoute, error) {
	if registry == nil {
		return ModelRoute{}, ErrModelNotFound
	}
	route, exists := registry.routes[protocol+"\x00"+model]
	if !exists {
		return ModelRoute{}, ErrModelNotFound
	}
	for _, capability := range route.Capabilities {
		if capability.Operation == operation && capability.MediaType == mediaType {
			route.Capabilities = append([]Capability(nil), route.Capabilities...)
			return route, nil
		}
	}
	return ModelRoute{}, ErrUnsupported
}

func (registry *Registry) List() []ModelRoute {
	return registry.ListProtocol("openai")
}

func (registry *Registry) ListProtocol(protocol string) []ModelRoute {
	models := []ModelRoute{}
	if registry == nil {
		return models
	}
	for _, route := range registry.routes {
		if route.Protocol != protocol {
			continue
		}
		route.Capabilities = append([]Capability(nil), route.Capabilities...)
		models = append(models, route)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Model < models[j].Model })
	return models
}

func validRouteProtocol(protocol string, provider providercredentials.ProviderID) bool {
	return (protocol == "openai" && (provider == providercredentials.OpenAI || provider == providercredentials.XAI)) || (protocol == "gemini" && provider == providercredentials.Google)
}

func validCapability(capability Capability) bool {
	return (capability.Operation == Generate && capability.MediaType == JSON) || (capability.Operation == Edit && (capability.MediaType == JSON || capability.MediaType == Multipart))
}
func validModelID(model string) bool {
	if model == "" || len(model) > 200 {
		return false
	}
	for _, character := range model {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return true
}

func validChannelID(channelID string) bool {
	if len(channelID) != len("channel_")+32 || !strings.HasPrefix(channelID, "channel_") {
		return false
	}
	for _, character := range strings.TrimPrefix(channelID, "channel_") {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}
