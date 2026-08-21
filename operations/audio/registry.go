// Package audio defines immutable audio operation routes.
package audio

import (
	"errors"
	"sort"
	"strings"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

const Speech = "audio.speech"

var ErrModelNotFound = errors.New("audio model not found")

type Model struct {
	ID, Owner, ProviderModel, ChannelID string
	Provider                            providercredentials.ProviderID
	Created                             int64
}

type Registry struct{ models map[string]Model }

func NewRegistry(ids []string) (*Registry, error) {
	models := make(map[string]Model, len(ids))
	for _, id := range ids {
		if id == "" || id != strings.TrimSpace(id) || len(id) > 200 || strings.ContainsAny(id, "\r\n") {
			return nil, ErrModelNotFound
		}
		if _, exists := models[id]; exists {
			return nil, ErrModelNotFound
		}
		models[id] = Model{ID: id, Owner: "openai", Provider: providercredentials.OpenAI, ProviderModel: id, ChannelID: "channel_00000000000000000000000000000001"}
	}
	return &Registry{models: models}, nil
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
	if registry == nil {
		return nil
	}
	result := make([]Model, 0, len(registry.models))
	for _, model := range registry.models {
		result = append(result, model)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
