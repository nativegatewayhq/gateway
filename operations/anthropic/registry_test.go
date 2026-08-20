package anthropic

import "testing"

func TestRegistryResolvesExactAnthropicModel(t *testing.T) {
	registry, err := NewRegistry([]string{"claude-sonnet-4-20250514"})
	if err != nil {
		t.Fatal(err)
	}
	model, err := registry.Resolve("claude-sonnet-4-20250514")
	if err != nil || model.ChannelID != "channel_00000000000000000000000000000006" || model.Provider != "anthropic" {
		t.Fatalf("model=%+v err=%v", model, err)
	}
	if _, err = NewRegistry([]string{"bad model"}); err == nil {
		t.Fatal("invalid model accepted")
	}
}
