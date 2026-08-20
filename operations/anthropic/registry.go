// Package anthropic defines Anthropic Messages capabilities.
package anthropic

import (
	"errors"
	"sort"
	"strings"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

const CreateMessage = "messages.create"

var (
	ErrModelNotFound = errors.New("Anthropic Messages model not found")
	ErrInvalidModel  = errors.New("invalid Anthropic Messages model")
)

type Model struct {
	ID, ProviderModel, ChannelID string
	Provider                     providercredentials.ProviderID
}

type Registry struct{ models map[string]Model }

func NewRegistry(ids []string) (*Registry, error) {
	registry := &Registry{models: make(map[string]Model, len(ids))}
	channelID, _ := providercredentials.LegacyChannel(providercredentials.Anthropic)
	for _, id := range ids {
		if !validID(id) {
			return nil, ErrInvalidModel
		}
		if _, exists := registry.models[id]; exists {
			return nil, ErrInvalidModel
		}
		registry.models[id] = Model{ID: id, ProviderModel: id, ChannelID: channelID, Provider: providercredentials.Anthropic}
	}
	return registry, nil
}

func (registry *Registry) Resolve(id string) (Model, error) {
	if registry == nil {
		return Model{}, ErrModelNotFound
	}
	model, ok := registry.models[id]
	if !ok {
		return Model{}, ErrModelNotFound
	}
	return model, nil
}

func (registry *Registry) List() []Model {
	result := make([]Model, 0, len(registry.models))
	for _, model := range registry.models {
		result = append(result, model)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result
}

func validID(value string) bool {
	if value == "" || len(value) > 200 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}
