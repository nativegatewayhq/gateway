package chat

import "testing"

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
