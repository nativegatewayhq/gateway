package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	asyncv1 "github.com/nativegatewayhq/gateway/plugin-sdk/async/v1"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
)

func newAsyncTestClient(t *testing.T, handler http.Handler) (*Client, Binding) {
	t.Helper()
	cfg := testConfig()
	cfg.ResultOrigins = map[string][]string{"provider.async-example": {"https://assets.example.com"}}
	registry, err := NewRegistry([]manifest.Validated{validatedAsync(t, "provider.async-example", "replicate", true)}, cfg)
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

type responseRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (recorder *responseRecorder) Header() http.Header    { return recorder.header }
func (recorder *responseRecorder) WriteHeader(status int) { recorder.status = status }
func (recorder *responseRecorder) Write(body []byte) (int, error) {
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	return recorder.body.Write(body)
}

func TestAsyncClientSubmitPollAndCancelUseFixedAuthenticatedPaths(t *testing.T) {
	client, binding := newAsyncTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" || request.Method != http.MethodPost {
			t.Error("missing fixed sidecar authentication")
		}
		switch request.URL.Path {
		case "/plugin/async/v1/submit":
			envelope, err := asyncv1.DecodeSubmitRequest(request.Body, 1<<20)
			if err != nil || envelope.CallbackURL == "" {
				t.Errorf("submit envelope=%#v err=%v", envelope, err)
				return
			}
			response := asyncv1.SubmitResponse{SchemaVersion: asyncv1.SubmitResponseSchema, RequestID: envelope.RequestID, GatewayJobID: envelope.GatewayJobID, PluginID: envelope.PluginID, PluginVersion: envelope.PluginVersion, ManifestDigest: envelope.ManifestDigest, ProviderJobRef: "provider:job-1", Observation: asyncv1.Observation{Status: "QUEUED"}}
			body, _ := asyncv1.CanonicalSubmitResponse(response, asyncv1.Expectation{Identity: envelope.Identity(), Output: "url", MaximumImages: 2})
			_, _ = writer.Write(body)
		case "/plugin/async/v1/poll", "/plugin/async/v1/cancel":
			envelope, err := asyncv1.DecodeControlRequest(request.Body, 1<<20)
			if err != nil || envelope.ProviderJobRef != "provider:job-1" {
				t.Errorf("control envelope=%#v err=%v", envelope, err)
				return
			}
			observation := asyncv1.Observation{Status: "CANCELED"}
			if envelope.Action == "poll" {
				observation = asyncv1.Observation{Status: "SUCCEEDED", Result: &asyncv1.Result{Images: []asyncv1.Image{{MIMEType: "image/png", URL: "https://assets.example.com/result.png"}}, Usage: asyncv1.Usage{Dimension: "output", Unit: "image", Quantity: 1}}}
			}
			response := asyncv1.ObservationResponse{SchemaVersion: asyncv1.ObservationResponseSchema, RequestID: envelope.RequestID, GatewayJobID: envelope.GatewayJobID, PluginID: envelope.PluginID, PluginVersion: envelope.PluginVersion, ManifestDigest: envelope.ManifestDigest, Observation: observation}
			body, _ := asyncv1.CanonicalObservationResponse(response, asyncv1.Expectation{Identity: envelope.Identity(), Output: "url", MaximumImages: 2})
			_, _ = writer.Write(body)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	jobID := "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	submit, err := client.SubmitAsync(context.Background(), binding.ChannelID, "request_1", jobID, "replicate", asyncv1.ImageInput{Prompt: "draw", Images: 1}, "https://gateway.example/internal/webhooks/plugin/job/token")
	if err != nil || submit.ProviderJobRef != "provider:job-1" {
		t.Fatalf("submit=%#v err=%v", submit, err)
	}
	poll, err := client.ControlAsync(context.Background(), binding.ChannelID, jobID+":poll", jobID, "poll", submit.ProviderJobRef)
	if err != nil || poll.Observation.Result == nil {
		t.Fatalf("poll=%#v err=%v", poll, err)
	}
	cancel, err := client.ControlAsync(context.Background(), binding.ChannelID, jobID+":cancel", jobID, "cancel", submit.ProviderJobRef)
	if err != nil || cancel.Observation.Status != "CANCELED" {
		t.Fatalf("cancel=%#v err=%v", cancel, err)
	}
}

func TestAsyncClientRejectsCrossOriginResultAndSyncBinding(t *testing.T) {
	resultURL := "https://evil.example/result.png"
	client, binding := newAsyncTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		envelope, _ := asyncv1.DecodeControlRequest(request.Body, 1<<20)
		response := asyncv1.ObservationResponse{SchemaVersion: asyncv1.ObservationResponseSchema, RequestID: envelope.RequestID, GatewayJobID: envelope.GatewayJobID, PluginID: envelope.PluginID, PluginVersion: envelope.PluginVersion, ManifestDigest: envelope.ManifestDigest, Observation: asyncv1.Observation{Status: "SUCCEEDED", Result: &asyncv1.Result{Images: []asyncv1.Image{{MIMEType: "image/png", URL: resultURL}}, Usage: asyncv1.Usage{Dimension: "output", Unit: "image", Quantity: 1}}}}
		body, _ := json.Marshal(response)
		_, _ = writer.Write(body)
	}))
	jobID := "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := client.ControlAsync(context.Background(), binding.ChannelID, jobID+":poll", jobID, "poll", "provider:job-1"); err != ErrInvalidResponse {
		t.Fatalf("cross-origin error=%v", err)
	}
	syncRegistry, err := NewRegistry([]manifest.Validated{validated(t, "provider.example", "openai")}, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	syncClient := NewClient(syncRegistry)
	if _, err = syncClient.ControlAsync(context.Background(), syncRegistry.Bindings()[0].ChannelID, jobID+":poll", jobID, "poll", "provider:job-1"); err != ErrInvalidRequest {
		t.Fatalf("sync binding accepted async control: %v", err)
	}
}
