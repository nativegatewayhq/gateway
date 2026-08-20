package telemetry

import (
	"net/http/httptest"
	"testing"
)

func TestReplicateWebhookMetadataUsesBoundedRouteWithoutCapability(t *testing.T) {
	request := httptest.NewRequest("POST", "https://gateway.example/internal/webhooks/replicate/job_00000000000000000000000000000000/whk_secretcapability", nil)
	protocol, operation, route := requestMetadata(request)
	if protocol != "replicate" || operation != "image.generate" || route != "/internal/webhooks/replicate/{job}/{token}" || boundedRoute(route) != route {
		t.Fatalf("metadata=%s/%s/%s", protocol, operation, route)
	}
}
