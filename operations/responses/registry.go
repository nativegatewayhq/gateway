// Package responses defines OpenAI Responses capabilities independently of Chat.
package responses

import (
	"errors"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"sort"
	"strings"
)

const Create = "responses.create"

var (
	ErrModelNotFound = errors.New("responses model not found")
	ErrInvalidModel  = errors.New("invalid responses model")
)

type Model struct {
	ID, Owner                string
	Created                  int64
	Provider                 providercredentials.ProviderID
	ProviderModel, ChannelID string
}
type Registry struct{ models map[string]Model }

func NewRegistry(ids []string) (*Registry, error) {
	r := &Registry{models: map[string]Model{}}
	for _, id := range ids {
		if id == "" || len(id) > 200 || strings.TrimSpace(id) != id {
			return nil, ErrInvalidModel
		}
		if _, ok := r.models[id]; ok {
			return nil, ErrInvalidModel
		}
		r.models[id] = Model{ID: id, Owner: "openai", Provider: providercredentials.OpenAI, ProviderModel: id, ChannelID: "channel_00000000000000000000000000000001"}
	}
	return r, nil
}
func (r *Registry) Resolve(id string) (Model, error) {
	if r == nil {
		return Model{}, ErrModelNotFound
	}
	m, ok := r.models[id]
	if !ok {
		return Model{}, ErrModelNotFound
	}
	return m, nil
}
func (r *Registry) List() []Model {
	result := make([]Model, 0, len(r.models))
	for _, m := range r.models {
		result = append(result, m)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
