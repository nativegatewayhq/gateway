package audio

import "testing"

func TestTranscriptionRegistryValidatesCapabilities(t *testing.T) {
	registry, err := NewTranscriptionRegistry([]string{"gpt-4o-transcribe"}, map[string]TranscriptionCapabilities{"gpt-4o-transcribe": {Streaming: true, ResponseFormats: []string{"json", "text"}}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := registry.Resolve("gpt-4o-transcribe")
	if err != nil || !model.Capabilities.Streaming || len(model.Capabilities.ResponseFormats) != 2 {
		t.Fatalf("model=%+v err=%v", model, err)
	}
	if _, err = NewTranscriptionRegistry([]string{"bad"}, map[string]TranscriptionCapabilities{"bad": {ResponseFormats: []string{"invented"}}}); err == nil {
		t.Fatal("invalid format accepted")
	}
}
