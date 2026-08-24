package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	videov1 "github.com/nativegatewayhq/gateway/plugin-sdk/video/v1"
)

func TestDeterministicSubmit(t *testing.T) {
	value := &server{token: "0123456789abcdef", callbackKey: bytes.Repeat([]byte{1}, 32), jobs: map[string]int{}}
	identity := videov1.Identity{RequestID: "request_1", GatewayJobID: "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PluginID: "provider.video-example", PluginVersion: "1.0.0", ManifestDigest: string(bytes.Repeat([]byte{'a'}, 64))}
	request := videov1.SubmitRequest{SchemaVersion: videov1.SubmitRequestSchema, RequestID: identity.RequestID, GatewayJobID: identity.GatewayJobID, PluginID: identity.PluginID, PluginVersion: identity.PluginVersion, ManifestDigest: identity.ManifestDigest, Protocol: "runway", Operation: "video.generate", Model: "example-video-v1", Input: videov1.Input{Kind: "text_to_video", Prompt: "animate", DurationSeconds: 5, Ratio: "16:9"}}
	body, _ := videov1.CanonicalSubmitRequest(request, expectation(identity))
	incoming := httptest.NewRequest(http.MethodPost, "/plugin/video/v1/submit", bytes.NewReader(body))
	incoming.Header.Set("Authorization", "Bearer "+value.token)
	response := httptest.NewRecorder()
	value.handler().ServeHTTP(response, incoming)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if _, err := videov1.DecodeSubmitResponse(response.Body, 1<<20, expectation(identity)); err != nil {
		t.Fatal(err)
	}
}
