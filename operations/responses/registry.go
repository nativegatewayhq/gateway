// Package responses defines immutable OpenAI Responses routes independently of Chat.
package responses

import (
	"errors"
	"sort"
	"strings"

	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	chatoperation "github.com/nativegatewayhq/gateway/operations/chat"
)

const Create = "responses.create"
const MaxRouteCandidates = chatoperation.MaxRouteCandidates

type Policy = chatoperation.Policy

const (
	Fixed      = chatoperation.Fixed
	Priority   = chatoperation.Priority
	Weighted   = chatoperation.Weighted
	LowestCost = chatoperation.LowestCost
)

var (
	ErrModelNotFound = errors.New("responses model not found")
	ErrInvalidModel  = errors.New("invalid responses model")
	ErrUnsupported   = errors.New("responses capability unsupported")
)

type Capabilities struct {
	Streaming, FunctionTools, WebSearch, XSearch, CodeInterpreter bool
	ImageGeneration, JSONMode, StoredResponse                     bool
}
type Requirements struct {
	Streaming, FunctionTools, WebSearch, XSearch, CodeInterpreter bool
	ImageGeneration, JSONMode, StoredResponse                     bool
}
type Candidate struct {
	ID                       string
	Provider                 providercredentials.ProviderID
	ProviderModel, ChannelID string
	Priority                 int
	Weight                   uint32
	Enabled                  bool
	Capabilities             Capabilities
}
type Route struct {
	Model, Owner                            string
	Created                                 int64
	Policy                                  Policy
	FixedCandidateID                        string
	MaximumInputTokens, MaximumOutputTokens int64
	Candidates                              []Candidate
}
type Model struct {
	ID, Owner                               string
	Created                                 int64
	Provider                                providercredentials.ProviderID
	ProviderModel, ChannelID, CandidateID   string
	Policy                                  Policy
	Priority                                int
	Weight                                  uint32
	Capabilities                            Capabilities
	MaximumInputTokens, MaximumOutputTokens int64
}
type Limits struct{ MaximumInputTokens, MaximumOutputTokens int64 }
type Registry struct{ routes map[string]Route }

func NewRegistry(ids []string) (*Registry, error) { return NewRegistryWithLimits(ids, nil) }
func NewRegistryWithLimits(ids []string, limits map[string]Limits) (*Registry, error) {
	routes := make([]Route, 0, len(ids))
	for _, id := range ids {
		limit := limits[id]
		candidateID := "candidate_openai"
		routes = append(routes, Route{Model: id, Owner: "openai", Policy: Fixed, FixedCandidateID: candidateID, MaximumInputTokens: limit.MaximumInputTokens, MaximumOutputTokens: limit.MaximumOutputTokens, Candidates: []Candidate{{ID: candidateID, Provider: providercredentials.OpenAI, ProviderModel: id, ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Weight: 1, Capabilities: Capabilities{Streaming: true, FunctionTools: true, WebSearch: true, CodeInterpreter: true, ImageGeneration: true, JSONMode: true, StoredResponse: true}}}})
	}
	for id := range limits {
		found := false
		for _, configured := range ids {
			if configured == id {
				found = true
				break
			}
		}
		if !found {
			return nil, ErrInvalidModel
		}
	}
	return NewRouteRegistry(routes)
}
func NewRouteRegistry(routes []Route) (*Registry, error) {
	registry := &Registry{routes: make(map[string]Route, len(routes))}
	for _, route := range routes {
		if !validRoute(route) {
			return nil, ErrInvalidModel
		}
		if _, exists := registry.routes[route.Model]; exists {
			return nil, ErrInvalidModel
		}
		copyRoute := route
		copyRoute.Candidates = append([]Candidate(nil), route.Candidates...)
		sort.Slice(copyRoute.Candidates, func(i, j int) bool {
			if copyRoute.Candidates[i].Priority != copyRoute.Candidates[j].Priority {
				return copyRoute.Candidates[i].Priority < copyRoute.Candidates[j].Priority
			}
			return copyRoute.Candidates[i].ID < copyRoute.Candidates[j].ID
		})
		registry.routes[route.Model] = copyRoute
	}
	return registry, nil
}
func (r *Registry) Resolve(id string) (Model, error) {
	candidates, err := r.Candidates(id, Requirements{})
	if err != nil {
		return Model{}, err
	}
	return candidates[0], nil
}
func (r *Registry) Candidates(id string, requirements Requirements) ([]Model, error) {
	if r == nil {
		return nil, ErrModelNotFound
	}
	route, ok := r.routes[id]
	if !ok {
		return nil, ErrModelNotFound
	}
	result := make([]Model, 0, len(route.Candidates))
	for _, candidate := range route.Candidates {
		if !candidate.Enabled || !supports(candidate.Capabilities, requirements) || (route.Policy == Fixed && candidate.ID != route.FixedCandidateID) {
			continue
		}
		result = append(result, decision(route, candidate))
	}
	if len(result) == 0 {
		return nil, ErrUnsupported
	}
	return result, nil
}
func (r *Registry) List() []Model {
	if r == nil {
		return nil
	}
	result := make([]Model, 0, len(r.routes))
	for _, route := range r.routes {
		for _, candidate := range route.Candidates {
			if candidate.Enabled && (route.Policy != Fixed || candidate.ID == route.FixedCandidateID) {
				result = append(result, decision(route, candidate))
				break
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
func OrderLowestCost(candidates []Model, quotes map[string]chatpricing.Estimate) ([]Model, error) {
	if len(candidates) == 0 || len(candidates) > MaxRouteCandidates {
		return nil, ErrInvalidModel
	}
	ordered := append([]Model(nil), candidates...)
	for _, candidate := range ordered {
		quote, ok := quotes[candidate.CandidateID]
		if !ok || quote.Price.ChannelID != candidate.ChannelID || quote.EstimatedCost < 0 || quote.MaximumSale < 1 {
			return nil, ErrInvalidModel
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		left, right := quotes[ordered[i].CandidateID], quotes[ordered[j].CandidateID]
		if left.EstimatedCost != right.EstimatedCost {
			return left.EstimatedCost < right.EstimatedCost
		}
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		return ordered[i].CandidateID < ordered[j].CandidateID
	})
	return ordered, nil
}

type WeightedSampler struct {
	inner *chatoperation.WeightedSampler
}

func DefaultWeightedSampler() *WeightedSampler {
	return &WeightedSampler{inner: chatoperation.DefaultWeightedSampler()}
}
func (s *WeightedSampler) Pick(candidates []Model) (Model, error) {
	if s == nil || s.inner == nil {
		return Model{}, chatoperation.ErrWeightedSampling
	}
	converted := make([]chatoperation.Model, 0, len(candidates))
	byID := make(map[string]Model, len(candidates))
	for _, candidate := range candidates {
		converted = append(converted, chatoperation.Model{CandidateID: candidate.CandidateID, Weight: candidate.Weight})
		byID[candidate.CandidateID] = candidate
	}
	picked, err := s.inner.Pick(converted)
	if err != nil {
		return Model{}, err
	}
	return byID[picked.CandidateID], nil
}
func supports(c Capabilities, r Requirements) bool {
	return (!r.Streaming || c.Streaming) && (!r.FunctionTools || c.FunctionTools) && (!r.WebSearch || c.WebSearch) && (!r.XSearch || c.XSearch) && (!r.CodeInterpreter || c.CodeInterpreter) && (!r.ImageGeneration || c.ImageGeneration) && (!r.JSONMode || c.JSONMode) && (!r.StoredResponse || c.StoredResponse)
}
func decision(route Route, candidate Candidate) Model {
	return Model{ID: route.Model, Owner: route.Owner, Created: route.Created, Provider: candidate.Provider, ProviderModel: candidate.ProviderModel, ChannelID: candidate.ChannelID, CandidateID: candidate.ID, Policy: route.Policy, Priority: candidate.Priority, Weight: candidate.Weight, Capabilities: candidate.Capabilities, MaximumInputTokens: route.MaximumInputTokens, MaximumOutputTokens: route.MaximumOutputTokens}
}
func validRoute(route Route) bool {
	if !validText(route.Model) || !validText(route.Owner) || len(route.Candidates) < 1 || len(route.Candidates) > MaxRouteCandidates || route.MaximumInputTokens < 0 || route.MaximumOutputTokens < 0 || (route.MaximumInputTokens == 0) != (route.MaximumOutputTokens == 0) {
		return false
	}
	if route.Policy != Fixed && route.Policy != Priority && route.Policy != Weighted && route.Policy != LowestCost {
		return false
	}
	seenID, seenChannel, fixed := map[string]bool{}, map[string]bool{}, false
	for _, candidate := range route.Candidates {
		if !validText(candidate.ID) || (candidate.Provider != providercredentials.OpenAI && candidate.Provider != providercredentials.XAI) || !validText(candidate.ProviderModel) || !validChannel(candidate.ChannelID) || candidate.Priority < 0 || seenID[candidate.ID] || seenChannel[candidate.ChannelID] {
			return false
		}
		if route.Policy == Weighted && (candidate.Weight < 1 || candidate.Weight > chatoperation.MaxCandidateWeight) {
			return false
		}
		if route.Policy != Weighted && candidate.Weight > 1 {
			return false
		}
		seenID[candidate.ID], seenChannel[candidate.ChannelID] = true, true
		if candidate.ID == route.FixedCandidateID {
			fixed = true
		}
	}
	return (route.Policy == Fixed && fixed) || (route.Policy != Fixed && route.FixedCandidateID == "")
}
func validText(value string) bool {
	if value == "" || len(value) > 200 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if char <= 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}
func validChannel(value string) bool {
	if len(value) != 40 || !strings.HasPrefix(value, "channel_") {
		return false
	}
	for _, char := range strings.TrimPrefix(value, "channel_") {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}
