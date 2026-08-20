// Package image defines immutable image operation capabilities.
package image

import (
	"crypto/sha256"
	"encoding/hex"
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
	Generate   Operation = "image.generate"
	Edit       Operation = "image.edit"
	JSON       MediaType = "application/json"
	Multipart  MediaType = "multipart/form-data"
	Fixed      Policy    = "fixed"
	Priority   Policy    = "priority"
	LowestCost Policy    = "lowest_cost"
	Weighted   Policy    = "weighted"

	MaxRouteCandidates = 128
	MaxCandidateWeight = uint32(1_000_000_000)
	MaxTotalWeight     = uint64(4_000_000_000)
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
type UsageCapability struct {
	Dimension, Unit, RequestExtractor, ResultExtractor string
	DefaultQuantity, MaximumQuantity                   int64
}
type ChannelCandidate struct {
	ID            string
	Provider      providercredentials.ProviderID
	ProviderModel string
	ChannelID     string
	Enabled       bool
	Priority      int
	Weight        uint32
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
	Usage            UsageCapability
}
type RoutingDecision struct {
	Protocol      string
	Model         string
	CandidateID   string
	Provider      providercredentials.ProviderID
	ProviderModel string
	ChannelID     string
	Policy        Policy
	Priority      int
	Weight        uint32
	Usage         UsageCapability
}
type Registry struct{ routes map[string]ModelRoute }

func NewRegistry(routes ...ModelRoute) (*Registry, error) {
	registry := &Registry{routes: make(map[string]ModelRoute, len(routes))}
	candidateIDs := map[string]struct{}{}
	for _, route := range routes {
		if !validProtocol(route.Protocol) || !validProtocolModelID(route.Protocol, route.Model) || strings.TrimSpace(route.Owner) == "" || route.Created < 0 || len(route.Capabilities) == 0 || len(route.Candidates) == 0 || len(route.Candidates) > MaxRouteCandidates || (route.Policy != Fixed && route.Policy != Priority && route.Policy != LowestCost && route.Policy != Weighted) || !validUsageCapability(route.Usage) {
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
		var totalWeight uint64
		for _, candidate := range route.Candidates {
			if !validCandidateID(candidate.ID) || !validRouteProtocol(route.Protocol, candidate.Provider) || !validProtocolModelID(route.Protocol, candidate.ProviderModel) || !validChannelID(candidate.ChannelID) || candidate.Priority < 0 || candidate.Weight > MaxCandidateWeight {
				return nil, ErrInvalidModel
			}
			if route.Policy == Weighted {
				if candidate.Enabled && candidate.Weight == 0 {
					return nil, ErrInvalidModel
				}
				if candidate.Enabled {
					totalWeight += uint64(candidate.Weight)
				}
			} else if candidate.Weight != 0 {
				return nil, ErrInvalidModel
			}
			if _, duplicate := candidateIDs[candidate.ID]; duplicate {
				return nil, ErrInvalidModel
			}
			candidateIDs[candidate.ID] = struct{}{}
			fixedFound = fixedFound || candidate.ID == route.FixedCandidateID
		}
		if (route.Policy == Fixed && !fixedFound) || (route.Policy != Fixed && route.FixedCandidateID != "") || (route.Policy == Weighted && (totalWeight == 0 || totalWeight > MaxTotalWeight)) {
			return nil, ErrInvalidModel
		}
		route.Capabilities = append([]Capability(nil), route.Capabilities...)
		route.Candidates = append([]ChannelCandidate(nil), route.Candidates...)
		registry.routes[key] = route
	}
	return registry, nil
}

func DefaultRegistry() *Registry {
	registry, err := NewRegistry(defaultRoutes()...)
	if err != nil {
		panic("invalid built-in image model registry")
	}
	return registry
}

func DefaultRegistryWithReplicate(models []string) (*Registry, error) {
	return DefaultRegistryWithAsync(models, nil)
}

func DefaultRegistryWithAsync(replicateModels, falModels []string) (*Registry, error) {
	routes := defaultRoutes()
	for _, model := range replicateModels {
		digest := sha256.Sum256([]byte(model))
		candidateID := "candidate_replicate_" + hex.EncodeToString(digest[:8])
		owner := "replicate"
		if before, _, ok := strings.Cut(model, "/"); ok && before != "" {
			owner = before
		}
		routes = append(routes, ModelRoute{Protocol: "replicate", Model: model, Owner: owner, Capabilities: []Capability{{Generate, JSON}}, Policy: Fixed, FixedCandidateID: candidateID, Candidates: []ChannelCandidate{{ID: candidateID, Provider: providercredentials.Replicate, ProviderModel: model, ChannelID: "channel_00000000000000000000000000000004", Enabled: true}}, Usage: UsageCapability{Dimension: "output", Unit: "image", DefaultQuantity: 1, MaximumQuantity: 10, RequestExtractor: "replicate-input-num_outputs-v1", ResultExtractor: "replicate-output-v1"}})
	}
	for _, model := range falModels {
		digest := sha256.Sum256([]byte(model))
		candidateID := "candidate_fal_" + hex.EncodeToString(digest[:8])
		owner, _, _ := strings.Cut(model, "/")
		routes = append(routes, ModelRoute{Protocol: "fal", Model: model, Owner: owner, Capabilities: []Capability{{Generate, JSON}}, Policy: Fixed, FixedCandidateID: candidateID, Candidates: []ChannelCandidate{{ID: candidateID, Provider: providercredentials.Fal, ProviderModel: model, ChannelID: "channel_00000000000000000000000000000005", Enabled: true}}, Usage: UsageCapability{Dimension: "output", Unit: "image", DefaultQuantity: 1, MaximumQuantity: 10, RequestExtractor: "fal-input-num_images-v1", ResultExtractor: "fal-output-v1"}})
	}
	return NewRegistry(routes...)
}

func defaultRoutes() []ModelRoute {
	return []ModelRoute{
		ModelRoute{Protocol: "openai", Model: "gpt-image-1", Owner: "openai", Capabilities: []Capability{{Generate, JSON}, {Edit, Multipart}}, Policy: Fixed, FixedCandidateID: "candidate_openai_primary", Candidates: []ChannelCandidate{{ID: "candidate_openai_primary", Provider: providercredentials.OpenAI, ProviderModel: "gpt-image-1", ChannelID: "channel_00000000000000000000000000000001", Enabled: true}}},
		ModelRoute{Protocol: "openai", Model: "grok-imagine-image-quality", Owner: "xai", Capabilities: []Capability{{Generate, JSON}, {Edit, JSON}}, Policy: Fixed, FixedCandidateID: "candidate_xai_primary", Candidates: []ChannelCandidate{{ID: "candidate_xai_primary", Provider: providercredentials.XAI, ProviderModel: "grok-imagine-image-quality", ChannelID: "channel_00000000000000000000000000000002", Enabled: true}}},
		ModelRoute{Protocol: "gemini", Model: "gemini-image", Owner: "google", Capabilities: []Capability{{Generate, JSON}}, Policy: Fixed, FixedCandidateID: "candidate_google_primary", Candidates: []ChannelCandidate{{ID: "candidate_google_primary", Provider: providercredentials.Google, ProviderModel: "gemini-image", ChannelID: "channel_00000000000000000000000000000003", Enabled: true}}},
	}
}

func (registry *Registry) Resolve(model string, operation Operation, mediaType MediaType) (RoutingDecision, error) {
	decisions, err := registry.Candidates("openai", model, operation, mediaType)
	if err != nil {
		return RoutingDecision{}, err
	}
	return decisions[0], nil
}

func (registry *Registry) ResolveProtocol(protocol, model string, operation Operation, mediaType MediaType) (RoutingDecision, error) {
	decisions, err := registry.Candidates(protocol, model, operation, mediaType)
	if err != nil {
		return RoutingDecision{}, err
	}
	return decisions[0], nil
}

func (registry *Registry) Candidates(protocol, model string, operation Operation, mediaType MediaType) ([]RoutingDecision, error) {
	if registry == nil {
		return nil, ErrModelNotFound
	}
	route, exists := registry.routes[protocol+"\x00"+model]
	if !exists {
		return nil, ErrModelNotFound
	}
	supported := false
	for _, capability := range route.Capabilities {
		if capability.Operation == operation && capability.MediaType == mediaType {
			supported = true
			break
		}
	}
	if !supported {
		return nil, ErrUnsupported
	}
	candidates := append([]ChannelCandidate(nil), route.Candidates...)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Priority == candidates[j].Priority {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].Priority < candidates[j].Priority
	})
	decisions := make([]RoutingDecision, 0, len(candidates))
	for _, candidate := range candidates {
		if !candidate.Enabled || (route.Policy == Fixed && candidate.ID != route.FixedCandidateID) {
			continue
		}
		decisions = append(decisions, RoutingDecision{Protocol: route.Protocol, Model: route.Model, CandidateID: candidate.ID, Provider: candidate.Provider, ProviderModel: candidate.ProviderModel, ChannelID: candidate.ChannelID, Policy: route.Policy, Priority: candidate.Priority, Weight: candidate.Weight, Usage: route.Usage})
	}
	if len(decisions) == 0 {
		return nil, ErrUnsupported
	}
	return decisions, nil
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

func validProtocol(protocol string) bool {
	return protocol == "openai" || protocol == "gemini" || protocol == "replicate" || protocol == "fal"
}

func validRouteProtocol(protocol string, provider providercredentials.ProviderID) bool {
	return (protocol == "openai" && (provider == providercredentials.OpenAI || provider == providercredentials.XAI)) || (protocol == "gemini" && provider == providercredentials.Google) || (protocol == "replicate" && provider == providercredentials.Replicate) || (protocol == "fal" && provider == providercredentials.Fal)
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
func validUsageCapability(value UsageCapability) bool {
	if value == (UsageCapability{}) {
		return true
	}
	return value.Dimension == "output" && value.Unit == "image" && value.DefaultQuantity >= 1 && value.DefaultQuantity <= value.MaximumQuantity && value.MaximumQuantity <= 10 && len(value.RequestExtractor) >= 1 && len(value.RequestExtractor) <= 80 && len(value.ResultExtractor) >= 1 && len(value.ResultExtractor) <= 80
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

func validProtocolModelID(protocol, model string) bool {
	if protocol == "fal" {
		if len(model) > 200 {
			return false
		}
		parts := strings.Split(model, "/")
		if len(parts) < 2 {
			return false
		}
		for _, part := range parts {
			if part == "." || part == ".." || !validModelID(part) {
				return false
			}
		}
		return true
	}
	if protocol != "replicate" {
		return validModelID(model)
	}
	ownerAndModel, version, ok := strings.Cut(model, ":")
	if !ok || strings.Contains(version, ":") || !validModelID(version) {
		return false
	}
	owner, name, ok := strings.Cut(ownerAndModel, "/")
	return ok && !strings.Contains(name, "/") && validModelID(owner) && validModelID(name)
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
