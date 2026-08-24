package video

import (
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"testing"
)

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

func TestRegistryAddsPluginVideoWithoutBuiltInShadowing(t *testing.T) {
	route := Route{Model: "plugin-video", ProviderModel: "plugin-video", Provider: providercredentials.Plugin, ChannelID: "channel_plugin", TextToVideo: true}
	registry, err := NewRegistryWithCapabilitiesAndAdditional([]string{"runway-video"}, nil, []Route{route})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.Resolve("plugin-video")
	if err != nil || resolved.Provider != providercredentials.Plugin {
		t.Fatalf("route=%#v err=%v", resolved, err)
	}
	route.Model = "runway-video"
	if _, err = NewRegistryWithCapabilitiesAndAdditional([]string{"runway-video"}, nil, []Route{route}); err == nil {
		t.Fatal("built-in shadow accepted")
	}
}
