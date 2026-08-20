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
		if !validModelID(route.Model) || !validChannelID(route.ChannelID) || strings.TrimSpace(route.Owner) == "" || route.Created < 0 || len(route.Capabilities) == 0 || (route.Provider != providercredentials.OpenAI && route.Provider != providercredentials.XAI) {
			return nil, ErrInvalidModel
		}
		if _, exists := registry.routes[route.Model]; exists {
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
		registry.routes[route.Model] = route
	}
	return registry, nil
}

func DefaultRegistry() *Registry {
	registry, err := NewRegistry(
		ModelRoute{Model: "gpt-image-1", Provider: providercredentials.OpenAI, ChannelID: "channel_00000000000000000000000000000001", Owner: "openai", Capabilities: []Capability{{Generate, JSON}, {Edit, Multipart}}},
		ModelRoute{Model: "grok-imagine-image-quality", Provider: providercredentials.XAI, ChannelID: "channel_00000000000000000000000000000002", Owner: "xai", Capabilities: []Capability{{Generate, JSON}, {Edit, JSON}}},
	)
	if err != nil {
		panic("invalid built-in image model registry")
	}
	return registry
}

func (registry *Registry) Resolve(model string, operation Operation, mediaType MediaType) (ModelRoute, error) {
	if registry == nil {
		return ModelRoute{}, ErrModelNotFound
	}
	route, exists := registry.routes[model]
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
	models := []ModelRoute{}
	if registry == nil {
		return models
	}
	for _, route := range registry.routes {
		route.Capabilities = append([]Capability(nil), route.Capabilities...)
		models = append(models, route)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Model < models[j].Model })
	return models
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
