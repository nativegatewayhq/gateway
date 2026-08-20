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

type Registry struct{ models map[string]struct{} }

func NewRegistry(ids []string) (*Registry, error) {
	registry := &Registry{models: make(map[string]struct{}, len(ids))}
	for _, id := range ids {
		if !validID(id) {
			return nil, ErrInvalidModel
		}
		if _, exists := registry.models[id]; exists {
			return nil, ErrInvalidModel
		}
		registry.models[id] = struct{}{}
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
