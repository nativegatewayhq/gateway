//go:build sdkconformance

package gemini

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	pluginruntime "github.com/nativegatewayhq/gateway/internal/plugins"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	pluginprovider "github.com/nativegatewayhq/gateway/providers/plugin"
)

type pluginRoundTrip func(*http.Request) (*http.Response, error)

func (function pluginRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestOfficialGeminiSDKsUsePluginWithOnlyBaseURLAndKey(t *testing.T) {
	validated, err := manifest.Parse([]byte(`{"schema_version":"nativegateway.provider/v1","id":"provider.example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"sidecar","auth_secret_ref":"token"},"models":[{"id":"example-image-v1","protocols":["gemini"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["base64"],"maximum_images":2}}]}`), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	plugins, err := pluginruntime.NewRegistry([]manifest.Validated{validated}, pluginruntime.Config{EndpointOrigins: map[string]string{"sidecar": "http://127.0.0.1:8081"}, AuthSecrets: map[string]string{"token": "sidecar-only"}, Timeout: time.Second, MaximumRequestBytes: 1 << 20, MaximumResponseBytes: 1 << 20, MaximumConcurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	client := pluginruntime.NewClientWithHTTPClient(plugins, &http.Client{Transport: pluginRoundTrip(func(request *http.Request) (*http.Response, error) {
		var envelope pluginruntime.ExecuteRequest
		if json.NewDecoder(request.Body).Decode(&envelope) != nil || envelope.Input.Prompt != "draw a cat" || request.Header.Get("Authorization") != "Bearer sidecar-only" {
			t.Fatal("invalid sidecar request")
		}
		encoded := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
		body, _ := json.Marshal(pluginruntime.ExecuteResponse{SchemaVersion: pluginruntime.ResponseSchema, RequestID: envelope.RequestID, PluginID: envelope.PluginID, PluginVersion: envelope.PluginVersion, ManifestDigest: envelope.ManifestDigest, Result: &pluginruntime.Result{Images: []pluginruntime.Image{{MIMEType: "image/png", Base64: encoded}}, Usage: pluginruntime.Usage{Images: 1}}})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})})
	models, err := imageoperation.DefaultRegistryWithAsyncAndAdditional(nil, nil, plugins.Routes())
	if err != nil {
		t.Fatal(err)
	}
	binding := plugins.Bindings()[0]
	handler := NewHandlerWithImageAndLLMModels(slog.Default(), &stubAuthenticator{principal: apikey.Principal{}}, models, pluginprovider.New(client), 1<<20, geminiChannelAvailability{binding.ChannelID: true}, providerhealth.NoopGate{}, nil)
	server := httptest.NewServer(handler)
	defer server.Close()
	python := `from google import genai
from google.genai import types
c=genai.Client(api_key="service-key",http_options=types.HttpOptions(base_url="` + server.URL + `"))
r=c.models.generate_content(model="example-image-v1",contents="draw a cat")
assert r.candidates[0].content.parts[0].inline_data.data.startswith(b"\x89PNG")`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/google-genai-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python plugin Gemini SDK: %v: %s", err, output)
	}
	javascript := `const {GoogleGenAI}=require("@google/genai");(async()=>{const c=new GoogleGenAI({apiKey:"service-key",httpOptions:{baseUrl:"` + server.URL + `"}});const r=await c.models.generateContent({model:"example-image-v1",contents:"draw a cat"});if(!r.candidates[0].content.parts[0].inlineData.data.startsWith("iVBOR"))process.exit(2)})().catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/google-genai-sdk-node/node_modules")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("JavaScript plugin Gemini SDK: %v: %s", err, output)
	}
}
