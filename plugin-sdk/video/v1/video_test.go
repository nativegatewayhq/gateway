package video

import (
	"bytes"
	"strings"
	"testing"
)

var testIdentity = Identity{RequestID: "request_1", GatewayJobID: "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PluginID: "provider.video-example", PluginVersion: "1.0.0", ManifestDigest: strings.Repeat("a", 64)}
var testExpectation = Expectation{Identity: testIdentity, MaximumDurationSeconds: 60, Ratios: map[string]bool{"16:9": true}, Audio: true, TextToVideo: true, ImageToVideo: true, ResultOrigins: map[string]bool{"https://assets.example.com": true}}

func TestVideoWireRoundTripAndBounds(t *testing.T) {
	seed := int64(7)
	request := SubmitRequest{SchemaVersion: SubmitRequestSchema, RequestID: testIdentity.RequestID, GatewayJobID: testIdentity.GatewayJobID, PluginID: testIdentity.PluginID, PluginVersion: testIdentity.PluginVersion, ManifestDigest: testIdentity.ManifestDigest, Protocol: "runway", Operation: "video.generate", Model: "example-video-v1", Input: Input{Kind: "image_to_video", Prompt: "animate", DurationSeconds: 10, Ratio: "16:9", Audio: true, Seed: &seed, Source: &SourceAsset{URI: "runway://asset_abcdefghijklmnop", ContentType: "image/png"}}, CallbackURL: "https://gateway.example/internal/webhooks/plugin-video/job/token"}
	body, err := CanonicalSubmitRequest(request, testExpectation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeSubmitRequest(bytes.NewReader(body), 1<<20, testExpectation); err != nil {
		t.Fatal(err)
	}
	response := ObservationResponse{SchemaVersion: ObservationResponseSchema, RequestID: testIdentity.RequestID, GatewayJobID: testIdentity.GatewayJobID, PluginID: testIdentity.PluginID, PluginVersion: testIdentity.PluginVersion, ManifestDigest: testIdentity.ManifestDigest, Observation: Observation{Status: "SUCCEEDED", Result: &Result{URL: "https://assets.example.com/video.mp4", ContentType: "video/mp4", DurationSeconds: 10}, Usage: &Usage{Dimension: "provider_credit", Unit: "microcredit", Quantity: 1250000}}}
	body, err = CanonicalObservationResponse(response, testExpectation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeObservationResponse(bytes.NewReader(body), 1<<20, testExpectation); err != nil {
		t.Fatal(err)
	}
}
func TestVideoWireRejectsOriginUsageAndModalityConflicts(t *testing.T) {
	values := []Observation{{Status: "SUCCEEDED", Result: &Result{URL: "https://evil.example/video.mp4", ContentType: "video/mp4", DurationSeconds: 10}, Usage: &Usage{Dimension: "provider_credit", Unit: "microcredit", Quantity: 1}}, {Status: "SUCCEEDED", Result: &Result{URL: "https://assets.example.com/video.mp4", ContentType: "video/mp4", DurationSeconds: 10}, Usage: &Usage{Dimension: "output", Unit: "image", Quantity: 1}}, {Status: "PROCESSING", Result: &Result{URL: "https://assets.example.com/video.mp4"}}}
	for _, value := range values {
		response := ObservationResponse{SchemaVersion: ObservationResponseSchema, RequestID: testIdentity.RequestID, GatewayJobID: testIdentity.GatewayJobID, PluginID: testIdentity.PluginID, PluginVersion: testIdentity.PluginVersion, ManifestDigest: testIdentity.ManifestDigest, Observation: value}
		if _, err := CanonicalObservationResponse(response, testExpectation); err == nil {
			t.Fatalf("accepted %#v", value)
		}
	}
	bad := SubmitRequest{SchemaVersion: SubmitRequestSchema, RequestID: testIdentity.RequestID, GatewayJobID: testIdentity.GatewayJobID, PluginID: testIdentity.PluginID, PluginVersion: testIdentity.PluginVersion, ManifestDigest: testIdentity.ManifestDigest, Protocol: "runway", Operation: "video.generate", Model: "example-video-v1", Input: Input{Kind: "text_to_video", Prompt: "draw", DurationSeconds: 5, Ratio: "16:9", Source: &SourceAsset{URI: "runway://asset_abcdefghijklmnop", ContentType: "image/png"}}}
	if _, err := CanonicalSubmitRequest(bad, testExpectation); err == nil {
		t.Fatal("text request accepted source")
	}
}
func TestVideoCallbackUsesPurposeSeparatedSignature(t *testing.T) {
	callback := Callback{SchemaVersion: CallbackSchema, DeliveryID: "delivery_abcdefghijklmnop", RequestID: testIdentity.RequestID, GatewayJobID: testIdentity.GatewayJobID, PluginID: testIdentity.PluginID, PluginVersion: testIdentity.PluginVersion, ManifestDigest: testIdentity.ManifestDigest, Protocol: "runway", Operation: "video.generate", Model: "example-video-v1", ProviderJobRef: "provider:task-1", Observation: Observation{Status: "CANCELED", Usage: &Usage{Dimension: "provider_credit", Unit: "microcredit", Quantity: 0}}}
	body, err := CanonicalCallback(callback, testExpectation)
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{3}, 32)
	signature, err := SignCallback(secret, 1700000000, callback.DeliveryID, body)
	if err != nil || VerifyCallbackSignature(secret, 1700000000, callback.DeliveryID, body, signature) != nil {
		t.Fatal("signature failed")
	}
	if VerifyCallbackSignature(secret, 1700000000, callback.DeliveryID, append(body, 'x'), signature) == nil {
		t.Fatal("tamper accepted")
	}
}
