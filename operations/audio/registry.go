// Package audio defines immutable audio operation routes.
package audio

import (
	"errors"
	"sort"
	"strings"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

const (
	Speech        = "audio.speech"
	Transcription = "audio.transcription"
	Translation   = "audio.translation"
)

var ErrModelNotFound = errors.New("audio model not found")

type Model struct {
	ID, Owner, ProviderModel, ChannelID string
	Provider                            providercredentials.ProviderID
	Created                             int64
}

type Registry struct{ models map[string]Model }

type TranscriptionCapabilities struct {
	Streaming       bool     `json:"streaming"`
	ResponseFormats []string `json:"response_formats"`
	Language        bool     `json:"language"`
	Prompt          bool     `json:"prompt"`
	Timestamps      bool     `json:"timestamps"`
}

type TranscriptionModel struct {
	Model
	Capabilities TranscriptionCapabilities
}

type TranscriptionRegistry struct{ models map[string]TranscriptionModel }

type TranslationCapabilities struct {
	ResponseFormats []string `json:"response_formats"`
	Prompt          bool     `json:"prompt"`
	Temperature     bool     `json:"temperature"`
}

type TranslationModel struct {
	Model
	Capabilities TranslationCapabilities
}

type TranslationRegistry struct{ models map[string]TranslationModel }

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

func NewTranscriptionRegistry(ids []string, capabilities map[string]TranscriptionCapabilities) (*TranscriptionRegistry, error) {
	models := make(map[string]TranscriptionModel, len(ids))
	for _, id := range ids {
		if id == "" || id != strings.TrimSpace(id) || len(id) > 200 || strings.ContainsAny(id, "\r\n") {
			return nil, ErrModelNotFound
		}
		if _, exists := models[id]; exists {
			return nil, ErrModelNotFound
		}
		capability := capabilities[id]
		seen := map[string]bool{}
		for _, format := range capability.ResponseFormats {
			if !validTranscriptionFormat(format) || seen[format] {
				return nil, ErrModelNotFound
			}
			seen[format] = true
		}
		if len(capability.ResponseFormats) == 0 {
			capability.ResponseFormats = []string{"json"}
		}
		models[id] = TranscriptionModel{Model: Model{ID: id, Owner: "openai", Provider: providercredentials.OpenAI, ProviderModel: id, ChannelID: "channel_00000000000000000000000000000001"}, Capabilities: capability}
	}
	for id := range capabilities {
		if _, ok := models[id]; !ok {
			return nil, ErrModelNotFound
		}
	}
	return &TranscriptionRegistry{models: models}, nil
}

func NewTranslationRegistry(ids []string, modelMap map[string]string, capabilities map[string]TranslationCapabilities) (*TranslationRegistry, error) {
	models := make(map[string]TranslationModel, len(ids))
	for _, id := range ids {
		if id == "" || id != strings.TrimSpace(id) || len(id) > 200 || strings.ContainsAny(id, "\r\n") {
			return nil, ErrModelNotFound
		}
		if _, exists := models[id]; exists {
			return nil, ErrModelNotFound
		}
		providerModel := id
		if mapped, ok := modelMap[id]; ok {
			if mapped == "" || mapped != strings.TrimSpace(mapped) || len(mapped) > 200 || strings.ContainsAny(mapped, "\r\n") {
				return nil, ErrModelNotFound
			}
			providerModel = mapped
		}
		capability := capabilities[id]
		seen := map[string]bool{}
		for _, format := range capability.ResponseFormats {
			if !validTranslationFormat(format) || seen[format] {
				return nil, ErrModelNotFound
			}
			seen[format] = true
		}
		if len(capability.ResponseFormats) == 0 {
			capability.ResponseFormats = []string{"json"}
		}
		models[id] = TranslationModel{Model: Model{ID: id, Owner: "openai", Provider: providercredentials.OpenAI, ProviderModel: providerModel, ChannelID: "channel_00000000000000000000000000000001"}, Capabilities: capability}
	}
	for id := range modelMap {
		if _, ok := models[id]; !ok {
			return nil, ErrModelNotFound
		}
	}
	for id := range capabilities {
		if _, ok := models[id]; !ok {
			return nil, ErrModelNotFound
		}
	}
	return &TranslationRegistry{models: models}, nil
}

func (registry *TranslationRegistry) Resolve(id string) (TranslationModel, error) {
	if registry == nil {
		return TranslationModel{}, ErrModelNotFound
	}
	model, ok := registry.models[id]
	if !ok {
		return TranslationModel{}, ErrModelNotFound
	}
	return model, nil
}

func (registry *TranslationRegistry) List() []TranslationModel {
	if registry == nil {
		return nil
	}
	result := make([]TranslationModel, 0, len(registry.models))
	for _, model := range registry.models {
		result = append(result, model)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (registry *TranscriptionRegistry) Resolve(id string) (TranscriptionModel, error) {
	if registry == nil {
		return TranscriptionModel{}, ErrModelNotFound
	}
	m, ok := registry.models[id]
	if !ok {
		return TranscriptionModel{}, ErrModelNotFound
	}
	return m, nil
}
func (registry *TranscriptionRegistry) List() []TranscriptionModel {
	if registry == nil {
		return nil
	}
	out := make([]TranscriptionModel, 0, len(registry.models))
	for _, m := range registry.models {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func validTranscriptionFormat(value string) bool {
	switch value {
	case "json", "text", "srt", "verbose_json", "vtt", "diarized_json":
		return true
	default:
		return false
	}
}

func validTranslationFormat(value string) bool {
	switch value {
	case "json", "text", "srt", "verbose_json", "vtt":
		return true
	default:
		return false
	}
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
