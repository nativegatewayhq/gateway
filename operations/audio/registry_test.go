package audio

import "testing"

func TestRegistryResolvesImmutableOpenAISpeechRoute(t *testing.T) {
	registry, err := NewRegistry([]string{"tts-1", "gpt-4o-mini-tts"})
	if err != nil {
		t.Fatal(err)
	}
	model, err := registry.Resolve("tts-1")
	if err != nil || model.Provider != "openai" || model.ProviderModel != "tts-1" || model.ChannelID == "" {
		t.Fatalf("model=%+v err=%v", model, err)
	}
	if _, err = NewRegistry([]string{"tts-1", "tts-1"}); err == nil {
		t.Fatal("duplicate model accepted")
	}
}
