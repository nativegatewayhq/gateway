package video

import "testing"

func TestRegistryRequiresExactModelCapability(t *testing.T) {
	registry, err := NewRegistryWithCapabilities([]string{"logical"}, map[string]ModelCapability{"logical": {ProviderModel: "gen4_turbo", TextToVideo: true}})
	if err != nil {
		t.Fatal(err)
	}
	route, err := registry.Resolve("logical")
	if err != nil || route.ProviderModel != "gen4_turbo" || !route.TextToVideo || route.ImageToVideo {
		t.Fatalf("route=%+v err=%v", route, err)
	}
	if _, err = registry.Resolve("Logical"); err == nil {
		t.Fatal("non-exact model accepted")
	}
	if _, err = NewRegistryWithCapabilities([]string{"logical"}, map[string]ModelCapability{"other": {TextToVideo: true}}); err == nil {
		t.Fatal("orphan capability accepted")
	}
}
