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

func TestTranslationRegistryMapsProviderModelAndCapabilities(t *testing.T) {
	registry, err := NewTranslationRegistry([]string{"translation-public"}, map[string]string{"translation-public": "whisper-1"}, map[string]TranslationCapabilities{"translation-public": {ResponseFormats: []string{"json", "text"}, Prompt: true, Temperature: true}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := registry.Resolve("translation-public")
	if err != nil || model.ProviderModel != "whisper-1" || !model.Capabilities.Prompt || len(model.Capabilities.ResponseFormats) != 2 {
		t.Fatalf("model=%+v err=%v", model, err)
	}
	if _, err = NewTranslationRegistry([]string{"translation-public"}, map[string]string{"unknown": "whisper-1"}, nil); err == nil {
		t.Fatal("unknown mapping accepted")
	}
	if _, err = NewTranslationRegistry([]string{"translation-public"}, nil, map[string]TranslationCapabilities{"translation-public": {ResponseFormats: []string{"diarized_json"}}}); err == nil {
		t.Fatal("transcription-only format accepted")
	}
}
