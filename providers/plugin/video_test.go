package plugin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/plugins"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
	videov1 "github.com/nativegatewayhq/gateway/plugin-sdk/video/v1"
	providerrunway "github.com/nativegatewayhq/gateway/providers/runway"
)

func TestVideoInputProjectsRunwayWithoutPassingProviderPayload(t *testing.T) {
	binding := plugins.Binding{MaximumDurationSeconds: 60, Ratios: map[string]struct{}{"1280:720": {}}}
	input, err := videoInput(providerrunway.SubmitPayload{Path: "/v1/image_to_video", Body: []byte(`{"model":"private-provider-model","promptImage":"runway://uploads/asset","promptText":"animate"}`)}, binding)
	if err != nil || input.Kind != "image_to_video" || input.DurationSeconds != 5 || input.Ratio != "1280:720" || input.Source == nil || input.Source.URI != "runway://uploads/asset" {
		t.Fatalf("input=%#v err=%v", input, err)
	}
	encoded, _ := json.Marshal(input)
	if string(encoded) == "" || contains(string(encoded), "private-provider-model") {
		t.Fatalf("provider payload leaked: %s", encoded)
	}
}

func TestVideoObservationUsesGatewayIdentityAndSettlementExtractor(t *testing.T) {
	value := videov1.Observation{Status: "SUCCEEDED", Result: &videov1.Result{URL: "https://assets.example.com/video.mp4", ContentType: "video/mp4", DurationSeconds: 5}, Usage: &videov1.Usage{Dimension: "provider_credit", Unit: "microcredit", Quantity: 750000}}
	result, err := videoObservation(value, "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "private:provider-ref", "poll")
	if err != nil || result.Status != joboperation.Succeeded || result.Usage == nil || result.Usage.ExtractorVersion != "runway-task-cost-v1" || result.Usage.Quantity != 750000 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if contains(string(result.Snapshot.Body), "private:provider-ref") || !contains(string(result.Snapshot.Body), "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatalf("identity projection=%s", result.Snapshot.Body)
	}
}

func contains(value, fragment string) bool { return strings.Contains(value, fragment) }
