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
	if route, _ := registry.Resolve("gpt-image-1", Generate, JSON); route.Capabilities[0].Operation != Generate {
		t.Fatal("registry was mutated")
	}
}

func TestRegistryRejectsInvalidManifest(t *testing.T) {
	t.Parallel()
	valid := ModelRoute{Protocol: "openai", Model: "model", Provider: providercredentials.OpenAI, ChannelID: "channel_00000000000000000000000000000001", Owner: "openai", Capabilities: []Capability{{Generate, JSON}}}
	if _, err := NewRegistry(valid, valid); !errors.Is(err, ErrDuplicateModel) {
		t.Fatalf("duplicate = %v", err)
	}
	for _, invalid := range []ModelRoute{
		{Protocol: "openai", Model: "", Provider: providercredentials.OpenAI, Owner: "openai", Capabilities: []Capability{{Generate, JSON}}},
		{Protocol: "gemini", Model: "model", Provider: providercredentials.Google, ChannelID: "bad", Owner: "google", Capabilities: []Capability{{Generate, JSON}}},
		{Protocol: "openai", Model: "model", Provider: providercredentials.OpenAI, Owner: "", Capabilities: []Capability{{Generate, JSON}}},
		{Protocol: "openai", Model: "model", Provider: providercredentials.OpenAI, Owner: "openai"},
		{Protocol: "openai", Model: "model", Provider: providercredentials.OpenAI, Owner: "openai", Capabilities: []Capability{{Generate, Multipart}}},
		{Protocol: "gemini", Model: "model", Provider: providercredentials.OpenAI, ChannelID: "channel_00000000000000000000000000000001", Owner: "openai", Capabilities: []Capability{{Generate, JSON}}},
	} {
		if _, err := NewRegistry(invalid); !errors.Is(err, ErrInvalidModel) {
			t.Fatalf("invalid accepted: %+v, %v", invalid, err)
		}
	}
}
