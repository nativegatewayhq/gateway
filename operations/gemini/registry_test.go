package gemini

import "testing"

func TestRegistryUsesExactConfiguredModels(t *testing.T) {
	registry, err := NewRegistry([]string{"gemini-2.5-pro", "gemini-2.5-flash"})
	if err != nil || !registry.Contains("gemini-2.5-pro") || registry.Contains("gemini-2.5") || len(registry.List()) != 2 {
		t.Fatalf("registry=%v err=%v", registry.List(), err)
	}
}

func TestRegistryValidatesLimits(t *testing.T) {
	registry, err := NewRegistryWithLimits([]string{"gemini-2.5-pro"}, map[string]Limits{"gemini-2.5-pro": {MaximumInputTokens: 1000, MaximumOutputTokens: 100}})
	model, resolveErr := registry.Resolve("gemini-2.5-pro")
	if err != nil || resolveErr != nil || model.MaximumOutputTokens != 100 {
		t.Fatalf("model=%+v err=%v resolve=%v", model, err, resolveErr)
	}
	if _, err := NewRegistryWithLimits([]string{"gemini-2.5-pro"}, map[string]Limits{"unknown": {1, 1}}); err == nil {
		t.Fatal("unknown model limits accepted")
	}
	if _, err := NewRegistryWithLimits([]string{"gemini-2.5-pro"}, map[string]Limits{"gemini-2.5-pro": {1, 0}}); err == nil {
		t.Fatal("partial limits accepted")
	}
}

func TestRegistryRejectsInvalidAndDuplicateModels(t *testing.T) {
	for _, models := range [][]string{{""}, {"bad model"}, {"gemini-2.5-pro", "gemini-2.5-pro"}} {
		if _, err := NewRegistry(models); err == nil {
			t.Fatalf("accepted %v", models)
		}
	}
}
