package chat

import (
	"bytes"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

func TestRegistryUsesExactUniqueModels(t *testing.T) {
	r, err := NewRegistry([]string{"gpt-4.1", "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	if model, err := r.Resolve("gpt-4.1"); err != nil || model.ProviderModel != "gpt-4.1" || model.ChannelID == "" {
		t.Fatalf("model=%+v err=%v", model, err)
	}
	if _, err := r.Resolve("GPT-4.1"); err == nil {
		t.Fatal("case-insensitive model lookup")
	}
	if _, err := NewRegistry([]string{"gpt-4.1", "gpt-4.1"}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := NewRegistry([]string{"bad model"}); err == nil {
		t.Fatal("invalid model accepted")
	}
}

func TestRouteRegistryFiltersCapabilitiesAndReturnsImmutableCandidates(t *testing.T) {
	r, err := NewRouteRegistry([]Route{{Model: "logical-chat", Owner: "gateway", Policy: Priority, MaximumInputTokens: 100, MaximumOutputTokens: 20, Candidates: []Candidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "gpt-4.1", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 2, Capabilities: Capabilities{Streaming: true, Tools: true}},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "grok-4", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 1, Capabilities: Capabilities{Streaming: true}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := r.Candidates("logical-chat", Requirements{Streaming: true, Tools: true})
	if err != nil || len(candidates) != 1 || candidates[0].Provider != providercredentials.OpenAI {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	candidates[0].ProviderModel = "mutated"
	again, _ := r.Candidates("logical-chat", Requirements{Streaming: true, Tools: true})
	if again[0].ProviderModel != "gpt-4.1" {
		t.Fatal("registry snapshot mutated")
	}
}

func TestRouteRegistryRejectsDuplicateChannelAndBadFixedRoute(t *testing.T) {
	base := Route{Model: "logical-chat", Owner: "gateway", Policy: Priority, Candidates: []Candidate{{ID: "candidate_a", Provider: providercredentials.OpenAI, ProviderModel: "a", ChannelID: "channel_00000000000000000000000000000001", Enabled: true}, {ID: "candidate_b", Provider: providercredentials.XAI, ProviderModel: "b", ChannelID: "channel_00000000000000000000000000000001", Enabled: true}}}
	if _, err := NewRouteRegistry([]Route{base}); err == nil {
		t.Fatal("duplicate channel accepted")
	}
	base.Policy, base.FixedCandidateID = Fixed, "missing"
	base.Candidates = base.Candidates[:1]
	if _, err := NewRouteRegistry([]Route{base}); err == nil {
		t.Fatal("missing fixed candidate accepted")
	}
}

func TestLowestCostOrderingUsesCostPriorityAndCandidateID(t *testing.T) {
	candidates := []Model{{CandidateID: "candidate_z", ChannelID: "channel_00000000000000000000000000000001", Priority: 0}, {CandidateID: "candidate_b", ChannelID: "channel_00000000000000000000000000000002", Priority: 2}, {CandidateID: "candidate_a", ChannelID: "channel_00000000000000000000000000000003", Priority: 2}}
	quotes := map[string]chatpricing.Estimate{}
	for _, candidate := range candidates {
		quotes[candidate.CandidateID] = chatpricing.Estimate{Price: chatpricing.Price{ChannelID: candidate.ChannelID}, EstimatedCost: 10, MaximumSale: 20}
	}
	quotes["candidate_z"] = chatpricing.Estimate{Price: chatpricing.Price{ChannelID: candidates[0].ChannelID}, EstimatedCost: 30, MaximumSale: 40}
	ordered, err := OrderLowestCost(candidates, quotes)
	if err != nil || ordered[0].CandidateID != "candidate_a" || ordered[1].CandidateID != "candidate_b" || ordered[2].CandidateID != "candidate_z" {
		t.Fatalf("ordered=%+v err=%v", ordered, err)
	}
}

func TestWeightedSamplerUsesCanonicalIntervals(t *testing.T) {
	sampler, _ := NewWeightedSampler(bytes.NewReader(make([]byte, 32)))
	selected, err := sampler.Pick([]Model{{CandidateID: "candidate_b", Weight: 2}, {CandidateID: "candidate_a", Weight: 1}})
	if err != nil || selected.CandidateID != "candidate_a" {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
}
