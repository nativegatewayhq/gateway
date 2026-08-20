// Package image defines image-operation routing metadata.
package image

import (
	"errors"
	"fmt"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

var (
	ErrModelNotFound  = errors.New("image model not found")
	ErrDuplicateModel = errors.New("duplicate image model")
)

// ModelRoute binds an exact public model identifier to one provider.
type ModelRoute struct {
	Model    string
	Provider providercredentials.ProviderID
}

// Registry is immutable after construction.
type Registry struct {
	routes map[string]providercredentials.ProviderID
}

func NewRegistry(routes ...ModelRoute) (*Registry, error) {
	registry := &Registry{routes: make(map[string]providercredentials.ProviderID, len(routes))}
	for _, route := range routes {
		if route.Model == "" {
			return nil, fmt.Errorf("%w: empty model", ErrModelNotFound)
		}
		if _, exists := registry.routes[route.Model]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateModel, route.Model)
		}
		if route.Provider != providercredentials.OpenAI && route.Provider != providercredentials.XAI {
			return nil, fmt.Errorf("%w: unsupported provider", ErrModelNotFound)
		}
		registry.routes[route.Model] = route.Provider
	}
	return registry, nil
}

func DefaultRegistry() *Registry {
	registry, err := NewRegistry(
		ModelRoute{Model: "gpt-image-1", Provider: providercredentials.OpenAI},
		ModelRoute{Model: "grok-imagine-image-quality", Provider: providercredentials.XAI},
	)
	if err != nil {
		panic("invalid built-in image model registry")
	}
	return registry
}

func (registry *Registry) ProviderForImageModel(model string) (providercredentials.ProviderID, error) {
	if registry == nil {
		return "", ErrModelNotFound
	}
	provider, exists := registry.routes[model]
	if !exists {
		return "", ErrModelNotFound
	}
	return provider, nil
}
