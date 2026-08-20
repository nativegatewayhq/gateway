// Package gemini defines Gemini generateContent operation capabilities.
package gemini

import (
	"errors"
	"sort"
	"strings"
)

const ChatCompletions = "chat.completions"

var (
	ErrModelNotFound = errors.New("gemini LLM model not found")
	ErrInvalidModel  = errors.New("invalid gemini LLM model")
)

type Limits struct{ MaximumInputTokens, MaximumOutputTokens int64 }
type Model struct {
	ID                                      string
	MaximumInputTokens, MaximumOutputTokens int64
}
type Registry struct{ models map[string]Model }

func NewRegistry(ids []string) (*Registry, error) {
	return NewRegistryWithLimits(ids, nil)
}

func NewRegistryWithLimits(ids []string, limits map[string]Limits) (*Registry, error) {
	registry := &Registry{models: make(map[string]Model, len(ids))}
	for _, id := range ids {
		if !validID(id) {
			return nil, ErrInvalidModel
		}
		if _, exists := registry.models[id]; exists {
			return nil, ErrInvalidModel
		}
		limit := limits[id]
		if limit.MaximumInputTokens < 0 || limit.MaximumOutputTokens < 0 || (limit.MaximumInputTokens == 0) != (limit.MaximumOutputTokens == 0) {
			return nil, ErrInvalidModel
		}
		registry.models[id] = Model{ID: id, MaximumInputTokens: limit.MaximumInputTokens, MaximumOutputTokens: limit.MaximumOutputTokens}
	}
	for id := range limits {
		if _, ok := registry.models[id]; !ok {
			return nil, ErrInvalidModel
		}
	}
	return registry, nil
}

func (registry *Registry) Contains(id string) bool {
	if registry == nil {
		return false
	}
	_, ok := registry.models[id]
	return ok
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

func (registry *Registry) List() []string {
	result := make([]string, 0, len(registry.models))
	for id := range registry.models {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func validID(value string) bool {
	if value == "" || len(value) > 200 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}
