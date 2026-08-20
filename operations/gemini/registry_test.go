package gemini

import "testing"

func TestRegistryUsesExactConfiguredModels(t *testing.T) {
	registry, err := NewRegistry([]string{"gemini-2.5-pro", "gemini-2.5-flash"})
	if err != nil || !registry.Contains("gemini-2.5-pro") || registry.Contains("gemini-2.5") || len(registry.List()) != 2 {
		t.Fatalf("registry=%v err=%v", registry.List(), err)
	}
}

func TestRegistryRejectsInvalidAndDuplicateModels(t *testing.T) {
	for _, models := range [][]string{{""}, {"bad model"}, {"gemini-2.5-pro", "gemini-2.5-pro"}} {
		if _, err := NewRegistry(models); err == nil {
			t.Fatalf("accepted %v", models)
		}
	}
}
