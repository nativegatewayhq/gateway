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
	models := registry.List()
	if len(models) != 2 || models[0].Model != "gpt-image-1" || models[1].Model != "grok-imagine-image-quality" {
		t.Fatalf("list = %+v", models)
	}
	models[0].Capabilities[0].Operation = "mutated"
	if route, _ := registry.Resolve("gpt-image-1", Generate, JSON); route.Capabilities[0].Operation != Generate {
		t.Fatal("registry was mutated")
	}
}

func TestRegistryRejectsInvalidManifest(t *testing.T) {
	t.Parallel()
	valid := ModelRoute{Model: "model", Provider: providercredentials.OpenAI, ChannelID: "channel_00000000000000000000000000000001", Owner: "openai", Capabilities: []Capability{{Generate, JSON}}}
	if _, err := NewRegistry(valid, valid); !errors.Is(err, ErrDuplicateModel) {
		t.Fatalf("duplicate = %v", err)
	}
	for _, invalid := range []ModelRoute{
		{Model: "", Provider: providercredentials.OpenAI, Owner: "openai", Capabilities: []Capability{{Generate, JSON}}},
		{Model: "model", Provider: providercredentials.Google, Owner: "google", Capabilities: []Capability{{Generate, JSON}}},
		{Model: "model", Provider: providercredentials.OpenAI, Owner: "", Capabilities: []Capability{{Generate, JSON}}},
		{Model: "model", Provider: providercredentials.OpenAI, Owner: "openai"},
		{Model: "model", Provider: providercredentials.OpenAI, Owner: "openai", Capabilities: []Capability{{Generate, Multipart}}},
	} {
		if _, err := NewRegistry(invalid); !errors.Is(err, ErrInvalidModel) {
			t.Fatalf("invalid accepted: %+v, %v", invalid, err)
		}
	}
}
