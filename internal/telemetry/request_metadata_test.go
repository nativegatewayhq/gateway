package telemetry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReplicateWebhookMetadataUsesBoundedRouteWithoutCapability(t *testing.T) {
	request := httptest.NewRequest("POST", "https://gateway.example/internal/webhooks/replicate/job_00000000000000000000000000000000/whk_secretcapability", nil)
	protocol, operation, route := requestMetadata(request)
	if protocol != "replicate" || operation != "image.generate" || route != "/internal/webhooks/replicate/{job}/{token}" || boundedRoute(route) != route {
		t.Fatalf("metadata=%s/%s/%s", protocol, operation, route)
	}
}

func TestAudioAssetMetadataUsesBoundedRouteWithoutAssetIdentity(t *testing.T) {
	for _, test := range []struct{ method, path, operation, route string }{{http.MethodPost, "https://gateway.example/v1/audio/assets", "audio.asset.create", "/v1/audio/assets"}, {http.MethodGet, "https://gateway.example/v1/audio/assets/audasset_private", "audio.asset.get", "/v1/audio/assets/{id}"}, {http.MethodDelete, "https://gateway.example/v1/audio/assets/audasset_private", "audio.asset.delete", "/v1/audio/assets/{id}"}} {
		request := httptest.NewRequest(test.method, test.path, nil)
		protocol, operation, route := requestMetadata(request)
		if protocol != "openai" || operation != test.operation || route != test.route || boundedOperation(operation) != operation || boundedRoute(route) != route || strings.Contains(route, "audasset_private") {
			t.Fatalf("metadata=%s/%s/%s", protocol, operation, route)
		}
	}
}

func TestSpeechAssetMetadataUsesBoundedRouteWithoutAssetIdentity(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://gateway.example/v1/audio/speech/assets/speechasset_private/content", nil)
	protocol, operation, route := requestMetadata(request)
	if protocol != "openai" || operation != "audio.speech.asset.content" || route != "/v1/audio/speech/assets/{id}/content" || strings.Contains(route, "private") || boundedOperation(operation) != operation || boundedRoute(route) != route {
		t.Fatalf("metadata=%s/%s/%s", protocol, operation, route)
	}
}

func TestFalWebhookMetadataUsesBoundedRouteWithoutCapability(t *testing.T) {
	request := httptest.NewRequest("POST", "https://gateway.example/internal/webhooks/fal/job_00000000000000000000000000000000/whk_secretcapability", nil)
	protocol, operation, route := requestMetadata(request)
	if protocol != "fal" || operation != "image.generate" || route != "/internal/webhooks/fal/{job}/{token}" || boundedRoute(route) != route {
		t.Fatalf("metadata=%s/%s/%s", protocol, operation, route)
	}
	if strings.Contains(route, "secretcapability") {
		t.Fatalf("route leaked capability: %q", route)
	}
}

func TestOpenAIChatMetadataIsBounded(t *testing.T) {
	request := httptest.NewRequest("POST", "https://gateway.example/v1/chat/completions", nil)
	protocol, operation, route := requestMetadata(request)
	if protocol != "openai" || operation != "chat.completions" || boundedOperation(operation) != operation || boundedRoute(route) != route {
		t.Fatalf("metadata=%s/%s/%s", protocol, operation, route)
	}
}
