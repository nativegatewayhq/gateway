package image

import (
	"errors"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

func TestRegistryUsesExactModelRoutes(t *testing.T) {
	t.Parallel()
	registry := DefaultRegistry()
	tests := []struct {
		model string
		want  providercredentials.ProviderID
	}{
		{model: "gpt-image-1", want: providercredentials.OpenAI},
		{model: "grok-imagine-image-quality", want: providercredentials.XAI},
	}
	for _, test := range tests {
		provider, err := registry.ProviderForImageModel(test.model)
		if err != nil || provider != test.want {
			t.Fatalf("route %q = %q, %v", test.model, provider, err)
		}
	}
	if _, err := registry.ProviderForImageModel("gpt-image-1-preview"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("unknown model error = %v", err)
	}
}

func TestRegistryRejectsDuplicateAndUnsupportedRoutes(t *testing.T) {
	t.Parallel()
	_, err := NewRegistry(
		ModelRoute{Model: "same", Provider: providercredentials.OpenAI},
		ModelRoute{Model: "same", Provider: providercredentials.XAI},
	)
	if !errors.Is(err, ErrDuplicateModel) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, err := NewRegistry(ModelRoute{Model: "model", Provider: providercredentials.Google}); err == nil {
		t.Fatal("unsupported provider accepted")
	}
}
