package telemetry

import (
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
