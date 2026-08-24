package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/plugins"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	"github.com/nativegatewayhq/gateway/providers/google"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func executorFixture(t *testing.T, protocol string) (*Executor, plugins.Binding) {
	t.Helper()
	body := []byte(`{"schema_version":"nativegateway.provider/v1","id":"provider.example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"sidecar","auth_secret_ref":"token"},"models":[{"id":"example-image-v1","protocols":["` + protocol + `"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["base64"],"maximum_images":2}}]}`)
	validated, err := manifest.Parse(body, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := plugins.NewRegistry([]manifest.Validated{validated}, plugins.Config{EndpointOrigins: map[string]string{"sidecar": "http://127.0.0.1:8081"}, AuthSecrets: map[string]string{"token": "sidecar-secret"}, ResultOrigins: map[string][]string{}, Timeout: time.Second, MaximumRequestBytes: 1 << 20, MaximumResponseBytes: 1 << 20, MaximumConcurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestBody, _ := io.ReadAll(request.Body)
		if request.Header.Get("Authorization") != "Bearer sidecar-secret" || bytes.Contains(requestBody, []byte("sidecar-secret")) {
			t.Error("credential boundary violated")
		}
		var envelope plugins.ExecuteRequest
		if json.Unmarshal(requestBody, &envelope) != nil || envelope.Input.Prompt != "draw a cat" {
			t.Errorf("unexpected canonical input: %s", requestBody)
		}
		encoded := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
		responseBody, _ := json.Marshal(plugins.ExecuteResponse{SchemaVersion: plugins.ResponseSchema, RequestID: envelope.RequestID, PluginID: envelope.PluginID, PluginVersion: envelope.PluginVersion, ManifestDigest: envelope.ManifestDigest, Result: &plugins.Result{Images: []plugins.Image{{MIMEType: "image/png", Base64: encoded}}, Usage: plugins.Usage{Images: 1}}})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(responseBody))}, nil
	})
	return New(plugins.NewClientWithHTTPClient(registry, &http.Client{Transport: transport})), registry.Bindings()[0]
}

func TestOpenAIProjection(t *testing.T) {
	executor, binding := executorFixture(t, "openai")
	response, err := executor.Generate(context.Background(), openaiimages.Request{Operation: openaiimages.Generate, ChannelID: binding.ChannelID, Body: strings.NewReader(`{"model":"example-image-v1","prompt":"draw a cat","n":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !bytes.Contains(body, []byte(`"b64_json"`)) {
		t.Fatalf("unexpected response: %d %s", response.StatusCode, body)
	}
}

func TestGeminiProjection(t *testing.T) {
	executor, binding := executorFixture(t, "gemini")
	response, err := executor.GenerateContent(context.Background(), google.GenerateContentRequest{Model: binding.Model, ChannelID: binding.ChannelID, Body: strings.NewReader(`{"contents":[{"parts":[{"text":"draw a cat"}]}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !bytes.Contains(body, []byte(`"inlineData"`)) {
		t.Fatalf("unexpected response: %d %s", response.StatusCode, body)
	}
}
