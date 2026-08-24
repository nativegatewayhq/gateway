package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	asyncv1 "github.com/nativegatewayhq/gateway/plugin-sdk/async/v1"
)

func TestDeterministicSubmit(t *testing.T) {
	value := &server{token: "0123456789abcdef", callbackKey: bytes.Repeat([]byte{1}, 32), jobs: map[string]int{}}
	identity := asyncv1.Identity{RequestID: "request_1", GatewayJobID: "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PluginID: "provider.async-example", PluginVersion: "1.0.0", ManifestDigest: string(bytes.Repeat([]byte{'a'}, 64))}
	request := asyncv1.SubmitRequest{SchemaVersion: asyncv1.SubmitRequestSchema, RequestID: identity.RequestID, GatewayJobID: identity.GatewayJobID, PluginID: identity.PluginID, PluginVersion: identity.PluginVersion, ManifestDigest: identity.ManifestDigest, Protocol: "replicate", Operation: "image.generate", Model: "example-async-image-v1", Input: asyncv1.ImageInput{Prompt: "draw", Images: 1}}
	body, _ := asyncv1.CanonicalSubmitRequest(request)
	incoming := httptest.NewRequest(http.MethodPost, "/plugin/async/v1/submit", bytes.NewReader(body))
	incoming.Header.Set("Authorization", "Bearer "+value.token)
	response := httptest.NewRecorder()
	value.handler().ServeHTTP(response, incoming)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if _, err := asyncv1.DecodeSubmitResponse(response.Body, 1<<20, asyncv1.Expectation{Identity: identity, Output: "base64", MaximumImages: 2}); err != nil {
		t.Fatal(err)
	}
}
