// Package chat defines immutable LLM chat capabilities independently of image operations.
package chat

import (
	"errors"
	"sort"
	"strings"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

const Completions = "chat.completions"

var (
	ErrModelNotFound = errors.New("chat model not found")
	ErrInvalidModel  = errors.New("invalid chat model")
)

type Model struct {
	ID                  string
	Owner               string
	Created             int64
	Provider            providercredentials.ProviderID
	ProviderModel       string
	ChannelID           string
	MaximumInputTokens  int64
	MaximumOutputTokens int64
}
type Limits struct{ MaximumInputTokens, MaximumOutputTokens int64 }

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
		registry.models[id] = Model{ID: id, Owner: "openai", Provider: providercredentials.OpenAI, ProviderModel: id, ChannelID: "channel_00000000000000000000000000000001", MaximumInputTokens: limit.MaximumInputTokens, MaximumOutputTokens: limit.MaximumOutputTokens}
	}
	for id := range limits {
		if _, ok := registry.models[id]; !ok {
			return nil, ErrInvalidModel
		}
	}
	return registry, nil
}

func (r *Registry) Resolve(id string) (Model, error) {
	if r == nil {
		return Model{}, ErrModelNotFound
	}
	model, ok := r.models[id]
	if !ok {
		return Model{}, ErrModelNotFound
	}
	return model, nil
}
func (r *Registry) List() []Model {
	result := make([]Model, 0, len(r.models))
	for _, m := range r.models {
		result = append(result, m)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
func validID(value string) bool {
	if value == "" || len(value) > 200 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
