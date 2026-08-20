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
type Policy string

const (
	Generate  Operation = "image.generate"
	Edit      Operation = "image.edit"
	JSON      MediaType = "application/json"
	Multipart MediaType = "multipart/form-data"
	Fixed     Policy    = "fixed"
	Priority  Policy    = "priority"
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
type ChannelCandidate struct {
	ID            string
	Provider      providercredentials.ProviderID
	ProviderModel string
	ChannelID     string
	Enabled       bool
	Priority      int
}
type ModelRoute struct {
	Protocol         string
	Model            string
	Owner            string
	Created          int64
	Capabilities     []Capability
	Policy           Policy
	FixedCandidateID string
	Candidates       []ChannelCandidate
}
type RoutingDecision struct {
	Protocol      string
	Model         string
	CandidateID   string
	Provider      providercredentials.ProviderID
	ProviderModel string
	ChannelID     string
	Policy        Policy
}
type Registry struct{ routes map[string]ModelRoute }

func NewRegistry(routes ...ModelRoute) (*Registry, error) {
	registry := &Registry{routes: make(map[string]ModelRoute, len(routes))}
	candidateIDs := map[string]struct{}{}
	channelIDs := map[string]struct{}{}
	for _, route := range routes {
		if !validProtocol(route.Protocol) || !validModelID(route.Model) || strings.TrimSpace(route.Owner) == "" || route.Created < 0 || len(route.Capabilities) == 0 || len(route.Candidates) == 0 || (route.Policy != Fixed && route.Policy != Priority) {
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
		fixedFound := false
		for _, candidate := range route.Candidates {
			if !validCandidateID(candidate.ID) || !validRouteProtocol(route.Protocol, candidate.Provider) || !validModelID(candidate.ProviderModel) || !validChannelID(candidate.ChannelID) || candidate.Priority < 0 {
				return nil, ErrInvalidModel
			}
			if _, duplicate := candidateIDs[candidate.ID]; duplicate {
				return nil, ErrInvalidModel
			}
			if _, duplicate := channelIDs[candidate.ChannelID]; duplicate {
				return nil, ErrInvalidModel
			}
			candidateIDs[candidate.ID] = struct{}{}
			channelIDs[candidate.ChannelID] = struct{}{}
			fixedFound = fixedFound || candidate.ID == route.FixedCandidateID
		}
		if (route.Policy == Fixed && !fixedFound) || (route.Policy == Priority && route.FixedCandidateID != "") {
			return nil, ErrInvalidModel
		}
		route.Capabilities = append([]Capability(nil), route.Capabilities...)
		route.Candidates = append([]ChannelCandidate(nil), route.Candidates...)
		registry.routes[key] = route
	}
	return registry, nil
}

func DefaultRegistry() *Registry {
	registry, err := NewRegistry(
		ModelRoute{Protocol: "openai", Model: "gpt-image-1", Owner: "openai", Capabilities: []Capability{{Generate, JSON}, {Edit, Multipart}}, Policy: Fixed, FixedCandidateID: "candidate_openai_primary", Candidates: []ChannelCandidate{{ID: "candidate_openai_primary", Provider: providercredentials.OpenAI, ProviderModel: "gpt-image-1", ChannelID: "channel_00000000000000000000000000000001", Enabled: true}}},
		ModelRoute{Protocol: "openai", Model: "grok-imagine-image-quality", Owner: "xai", Capabilities: []Capability{{Generate, JSON}, {Edit, JSON}}, Policy: Fixed, FixedCandidateID: "candidate_xai_primary", Candidates: []ChannelCandidate{{ID: "candidate_xai_primary", Provider: providercredentials.XAI, ProviderModel: "grok-imagine-image-quality", ChannelID: "channel_00000000000000000000000000000002", Enabled: true}}},
		ModelRoute{Protocol: "gemini", Model: "gemini-image", Owner: "google", Capabilities: []Capability{{Generate, JSON}}, Policy: Fixed, FixedCandidateID: "candidate_google_primary", Candidates: []ChannelCandidate{{ID: "candidate_google_primary", Provider: providercredentials.Google, ProviderModel: "gemini-image", ChannelID: "channel_00000000000000000000000000000003", Enabled: true}}},
	)
	if err != nil {
		panic("invalid built-in image model registry")
	}
	return registry
}

func (registry *Registry) Resolve(model string, operation Operation, mediaType MediaType) (RoutingDecision, error) {
	return registry.ResolveProtocol("openai", model, operation, mediaType)
}

func (registry *Registry) ResolveProtocol(protocol, model string, operation Operation, mediaType MediaType) (RoutingDecision, error) {
	if registry == nil {
		return RoutingDecision{}, ErrModelNotFound
	}
	route, exists := registry.routes[protocol+"\x00"+model]
	if !exists {
		return RoutingDecision{}, ErrModelNotFound
	}
	supported := false
	for _, capability := range route.Capabilities {
		if capability.Operation == operation && capability.MediaType == mediaType {
			supported = true
			break
		}
	}
	if !supported {
		return RoutingDecision{}, ErrUnsupported
	}
	candidates := append([]ChannelCandidate(nil), route.Candidates...)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Priority == candidates[j].Priority {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].Priority < candidates[j].Priority
	})
	for _, candidate := range candidates {
		if !candidate.Enabled || (route.Policy == Fixed && candidate.ID != route.FixedCandidateID) {
			continue
		}
		return RoutingDecision{Protocol: route.Protocol, Model: route.Model, CandidateID: candidate.ID, Provider: candidate.Provider, ProviderModel: candidate.ProviderModel, ChannelID: candidate.ChannelID, Policy: route.Policy}, nil
	}
	return RoutingDecision{}, ErrUnsupported
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
		route.Candidates = append([]ChannelCandidate(nil), route.Candidates...)
		models = append(models, route)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Model < models[j].Model })
	return models
}

func validProtocol(protocol string) bool { return protocol == "openai" || protocol == "gemini" }

func validRouteProtocol(protocol string, provider providercredentials.ProviderID) bool {
	return (protocol == "openai" && (provider == providercredentials.OpenAI || provider == providercredentials.XAI)) || (protocol == "gemini" && provider == providercredentials.Google)
}

func validCandidateID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
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
