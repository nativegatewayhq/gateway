package responses

import (
	"testing"

	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

const (
	openAIChannel = "channel_00000000000000000000000000000001"
	xAIChannel    = "channel_00000000000000000000000000000002"
)

func routedRegistry(t *testing.T, policy Policy) *Registry {
	t.Helper()
	route := Route{Model: "logical-responses", Owner: "gateway", Policy: policy, MaximumInputTokens: 4096, MaximumOutputTokens: 512, Candidates: []Candidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "gpt-4.1", ChannelID: openAIChannel, Enabled: true, Weight: 1, Priority: 1, Capabilities: Capabilities{Streaming: true, FunctionTools: true, WebSearch: true, JSONMode: true, StoredResponse: true}},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "grok-4", ChannelID: xAIChannel, Enabled: true, Weight: 1, Priority: 0, Capabilities: Capabilities{Streaming: true, FunctionTools: true, XSearch: true, CodeInterpreter: true}},
	}}
	if policy == Weighted {
		route.Candidates[1].Weight = 3
	}
	if policy == Fixed {
		route.FixedCandidateID = "candidate_openai"
	}
	registry, err := NewRouteRegistry([]Route{route})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestRouteRegistryFiltersExactResponsesCapabilities(t *testing.T) {
	registry := routedRegistry(t, Priority)
	candidates, err := registry.Candidates("logical-responses", Requirements{XSearch: true, Streaming: true})
	if err != nil || len(candidates) != 1 || candidates[0].Provider != providercredentials.XAI {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	if _, err = registry.Candidates("logical-responses", Requirements{ImageGeneration: true}); err != ErrUnsupported {
		t.Fatalf("err=%v", err)
	}
}

func TestRouteRegistryIsImmutableAndValidatesDuplicates(t *testing.T) {
	route := Route{Model: "logical", Owner: "gateway", Policy: Priority, Candidates: []Candidate{{ID: "a", Provider: providercredentials.OpenAI, ProviderModel: "gpt", ChannelID: openAIChannel, Enabled: true}, {ID: "b", Provider: providercredentials.XAI, ProviderModel: "grok", ChannelID: xAIChannel, Enabled: true}}}
	registry, err := NewRouteRegistry([]Route{route})
	if err != nil {
		t.Fatal(err)
	}
	route.Candidates[0].ProviderModel = "mutated"
	resolved, _ := registry.Resolve("logical")
	if resolved.ProviderModel == "mutated" {
		t.Fatal("registry retained mutable input")
	}
	route.Candidates[1].ChannelID = openAIChannel
	if _, err = NewRouteRegistry([]Route{route}); err != ErrInvalidModel {
		t.Fatalf("duplicate channel err=%v", err)
	}
}

func TestLowestCostOrdersWithDeterministicTies(t *testing.T) {
	candidates, _ := routedRegistry(t, LowestCost).Candidates("logical-responses", Requirements{})
	quotes := map[string]chatpricing.Estimate{
		"candidate_openai": {EstimatedCost: 8, MaximumSale: 10, Price: chatpricing.Price{ChannelID: openAIChannel}},
		"candidate_xai":    {EstimatedCost: 4, MaximumSale: 10, Price: chatpricing.Price{ChannelID: xAIChannel}},
	}
	ordered, err := OrderLowestCost(candidates, quotes)
	if err != nil || ordered[0].CandidateID != "candidate_xai" {
		t.Fatalf("ordered=%+v err=%v", ordered, err)
	}
}

func TestFixedAndWeightedPoliciesReturnValidCandidates(t *testing.T) {
	fixed := routedRegistry(t, Fixed)
	candidates, err := fixed.Candidates("logical-responses", Requirements{})
	if err != nil || len(candidates) != 1 || candidates[0].CandidateID != "candidate_openai" {
		t.Fatalf("fixed=%+v err=%v", candidates, err)
	}
	weightedCandidates, err := routedRegistry(t, Weighted).Candidates("logical-responses", Requirements{})
	if err != nil {
		t.Fatal(err)
	}
	picked, err := DefaultWeightedSampler().Pick(weightedCandidates)
	if err != nil || (picked.CandidateID != "candidate_openai" && picked.CandidateID != "candidate_xai") {
		t.Fatalf("picked=%+v err=%v", picked, err)
	}
}
