package plugins

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	videov1 "github.com/nativegatewayhq/gateway/plugin-sdk/video/v1"
)

func newVideoTestClient(t *testing.T, handler http.Handler) (*Client, Binding) {
	t.Helper()
	cfg := testConfig()
	cfg.ResultOrigins = map[string][]string{"provider.video-example": {"https://assets.example.com"}}
	registry, err := NewRegistry([]manifest.Validated{validatedVideo(t, "provider.video-example", true)}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := &responseRecorder{header: make(http.Header)}
		handler.ServeHTTP(recorder, request)
		return &http.Response{StatusCode: recorder.status, Header: recorder.header, Body: io.NopCloser(bytes.NewReader(recorder.body.Bytes()))}, nil
	})}
	return NewClientWithHTTPClient(registry, httpClient), registry.Bindings()[0]
}

func TestVideoClientUsesIsolatedFixedEndpointsAndValidatesResultOrigin(t *testing.T) {
	var bindingDigest string
	client, binding := newVideoTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing auth")
		}
		switch request.URL.Path {
		case "/plugin/video/v1/submit":
			identity := videov1.Identity{RequestID: "request_1", GatewayJobID: "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PluginID: "provider.video-example", PluginVersion: "1.0.0", ManifestDigest: bindingDigest}
			expected := videov1.Expectation{Identity: identity, MaximumDurationSeconds: 60, Ratios: map[string]bool{"16:9": true, "9:16": true}, Audio: true, TextToVideo: true, ImageToVideo: true, ResultOrigins: map[string]bool{"https://assets.example.com": true}}
			envelope, err := videov1.DecodeSubmitRequest(request.Body, 1<<20, expected)
			if err != nil || envelope.Input.Kind != "text_to_video" || envelope.CallbackURL == "" {
				t.Fatalf("submit=%#v err=%v", envelope, err)
			}
			response := videov1.SubmitResponse{SchemaVersion: videov1.SubmitResponseSchema, RequestID: envelope.RequestID, GatewayJobID: envelope.GatewayJobID, PluginID: envelope.PluginID, PluginVersion: envelope.PluginVersion, ManifestDigest: envelope.ManifestDigest, ProviderJobRef: "provider:video-1", Observation: videov1.Observation{Status: "QUEUED"}}
			body, _ := videov1.CanonicalSubmitResponse(response, expected)
			_, _ = writer.Write(body)
		case "/plugin/video/v1/poll":
			envelope, err := videov1.DecodeControlRequest(request.Body, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			expected := videov1.Expectation{Identity: envelope.Identity(), MaximumDurationSeconds: 60, Ratios: map[string]bool{"16:9": true, "9:16": true}, Audio: true, TextToVideo: true, ImageToVideo: true, ResultOrigins: map[string]bool{"https://assets.example.com": true}}
			response := videov1.ObservationResponse{SchemaVersion: videov1.ObservationResponseSchema, RequestID: envelope.RequestID, GatewayJobID: envelope.GatewayJobID, PluginID: envelope.PluginID, PluginVersion: envelope.PluginVersion, ManifestDigest: envelope.ManifestDigest, Observation: videov1.Observation{Status: "SUCCEEDED", Result: &videov1.Result{URL: "https://assets.example.com/result.mp4", ContentType: "video/mp4", DurationSeconds: 5}, Usage: &videov1.Usage{Dimension: "provider_credit", Unit: "microcredit", Quantity: 2_500_000}}}
			body, _ := videov1.CanonicalObservationResponse(response, expected)
			_, _ = writer.Write(body)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	bindingDigest = binding.DigestHex()
	jobID := "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	input := videov1.Input{Kind: "text_to_video", Prompt: "animate", DurationSeconds: 5, Ratio: "16:9"}
	submit, err := client.SubmitVideo(context.Background(), binding.ChannelID, "request_1", jobID, input, "https://gateway.example/internal/webhooks/plugin-video/job/token")
	if err != nil || submit.ProviderJobRef != "provider:video-1" {
		t.Fatalf("submit=%#v err=%v", submit, err)
	}
	poll, err := client.ControlVideo(context.Background(), binding.ChannelID, jobID+":poll", jobID, "poll", submit.ProviderJobRef)
	if err != nil || poll.Observation.Result == nil || poll.Observation.Usage.Quantity != 2_500_000 {
		t.Fatalf("poll=%#v err=%v", poll, err)
	}
}
