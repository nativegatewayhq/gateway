package image

import (
	"errors"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

func TestRegistryResolvesCapabilitiesAndListsStably(t *testing.T) {
	t.Parallel()
	registry := DefaultRegistry()
	for _, test := range []struct {
		model     string
		operation Operation
		media     MediaType
		provider  providercredentials.ProviderID
	}{
		{"gpt-image-1", Generate, JSON, providercredentials.OpenAI},
		{"gpt-image-1", Edit, Multipart, providercredentials.OpenAI},
		{"grok-imagine-image-quality", Edit, JSON, providercredentials.XAI},
	} {
		route, err := registry.Resolve(test.model, test.operation, test.media)
		if err != nil || route.Provider != test.provider {
			t.Fatalf("resolve = %+v, %v", route, err)
		}
	}
	if _, err := registry.Resolve("gpt-image-1", Edit, JSON); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported = %v", err)
	}
	if _, err := registry.Resolve("missing", Generate, JSON); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if _, err := registry.Resolve("gemini-image", Generate, JSON); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("cross-protocol model leaked into OpenAI: %v", err)
	}
	models := registry.List()
	if len(models) != 2 || models[0].Model != "gpt-image-1" || models[1].Model != "grok-imagine-image-quality" {
		t.Fatalf("list = %+v", models)
	}
	if route, err := registry.ResolveProtocol("gemini", "gemini-image", Generate, JSON); err != nil || route.Provider != providercredentials.Google {
		t.Fatalf("gemini route=%+v error=%v", route, err)
	}
	if geminiModels := registry.ListProtocol("gemini"); len(geminiModels) != 1 || geminiModels[0].Model != "gemini-image" {
		t.Fatalf("gemini list=%+v", geminiModels)
	}
	models[0].Capabilities[0].Operation = "mutated"
	models[0].Candidates[0].ProviderModel = "mutated"
	if listed := registry.List(); listed[0].Capabilities[0].Operation == "mutated" || listed[0].Candidates[0].ProviderModel == "mutated" {
		t.Fatal("registry was mutated")
	}
}

func TestRegistryRejectsInvalidManifest(t *testing.T) {
	t.Parallel()
	valid := ModelRoute{Protocol: "openai", Model: "model", Owner: "openai", Capabilities: []Capability{{Generate, JSON}}, Policy: Fixed, FixedCandidateID: "candidate_one", Candidates: []ChannelCandidate{{ID: "candidate_one", Provider: providercredentials.OpenAI, ProviderModel: "model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true}}}
	if _, err := NewRegistry(valid, valid); !errors.Is(err, ErrDuplicateModel) {
		t.Fatalf("duplicate = %v", err)
	}
	duplicateChannel := valid
	duplicateChannel.Model = "other-model"
	duplicateChannel.FixedCandidateID = "candidate_other"
	duplicateChannel.Candidates = []ChannelCandidate{{ID: "candidate_other", Provider: providercredentials.OpenAI, ProviderModel: "other-model", ChannelID: valid.Candidates[0].ChannelID, Enabled: true}}
	if _, err := NewRegistry(valid, duplicateChannel); !errors.Is(err, ErrInvalidModel) {
		t.Fatalf("duplicate channel=%v", err)
	}
	for _, invalid := range []ModelRoute{
		{},
		{Protocol: "openai", Model: "model", Owner: "openai", Capabilities: []Capability{{Generate, JSON}}, Policy: Fixed, FixedCandidateID: "missing", Candidates: valid.Candidates},
		{Protocol: "openai", Model: "model", Owner: "openai", Capabilities: []Capability{{Generate, Multipart}}, Policy: Fixed, FixedCandidateID: "candidate_one", Candidates: valid.Candidates},
		{Protocol: "gemini", Model: "model", Owner: "google", Capabilities: []Capability{{Generate, JSON}}, Policy: Fixed, FixedCandidateID: "candidate_one", Candidates: valid.Candidates},
		{Protocol: "openai", Model: "model", Owner: "openai", Capabilities: []Capability{{Generate, JSON}}, Policy: Priority, FixedCandidateID: "candidate_one", Candidates: valid.Candidates},
	} {
		if _, err := NewRegistry(invalid); !errors.Is(err, ErrInvalidModel) {
			t.Fatalf("invalid accepted: %+v, %v", invalid, err)
		}
	}
}

func TestPriorityRoutingIsDeterministicAndFiltersDisabled(t *testing.T) {
	route := ModelRoute{Protocol: "openai", Model: "logical-image", Owner: "gateway", Capabilities: []Capability{{Generate, JSON}}, Policy: Priority, Candidates: []ChannelCandidate{
		{ID: "candidate_z", Provider: providercredentials.OpenAI, ProviderModel: "provider-z", ChannelID: "channel_00000000000000000000000000000004", Enabled: true, Priority: 10},
		{ID: "candidate_b", Provider: providercredentials.XAI, ProviderModel: "provider-b", ChannelID: "channel_00000000000000000000000000000005", Enabled: true, Priority: 1},
		{ID: "candidate_a", Provider: providercredentials.OpenAI, ProviderModel: "provider-a", ChannelID: "channel_00000000000000000000000000000006", Enabled: true, Priority: 1},
		{ID: "candidate_disabled", Provider: providercredentials.XAI, ProviderModel: "disabled", ChannelID: "channel_00000000000000000000000000000007", Enabled: false, Priority: 0},
	}}
	registry, err := NewRegistry(route)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := registry.Resolve("logical-image", Generate, JSON)
	if err != nil || decision.CandidateID != "candidate_a" || decision.ProviderModel != "provider-a" || decision.Policy != Priority {
		t.Fatalf("decision=%+v error=%v", decision, err)
	}
}

func TestFixedRoutingDoesNotSelectAnAlternateCandidate(t *testing.T) {
	registry, err := NewRegistry(ModelRoute{Protocol: "openai", Model: "fixed-image", Owner: "gateway", Capabilities: []Capability{{Generate, JSON}}, Policy: Fixed, FixedCandidateID: "candidate_fixed", Candidates: []ChannelCandidate{
		{ID: "candidate_fixed", Provider: providercredentials.OpenAI, ProviderModel: "fixed", ChannelID: "channel_00000000000000000000000000000008", Enabled: false},
		{ID: "candidate_alternate", Provider: providercredentials.XAI, ProviderModel: "alternate", ChannelID: "channel_00000000000000000000000000000009", Enabled: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve("fixed-image", Generate, JSON); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("fixed route selected alternate: %v", err)
	}
}
