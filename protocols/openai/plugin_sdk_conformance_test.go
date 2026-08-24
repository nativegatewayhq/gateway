//go:build sdkconformance

package openai

import (
	"bytes"
	"context"
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
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	pluginprovider "github.com/nativegatewayhq/gateway/providers/plugin"
)

type pluginRoundTrip func(*http.Request) (*http.Response, error)

func (function pluginRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestOfficialOpenAIImageSDKsUsePluginWithOnlyBaseURLAndKey(t *testing.T) {
	validated, err := manifest.Parse([]byte(`{"schema_version":"nativegateway.provider/v1","id":"provider.example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"sidecar","auth_secret_ref":"token"},"models":[{"id":"example-image-v1","protocols":["openai"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["base64"],"maximum_images":2}}]}`), "0.1.0")
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
	handler := NewImagesHandler(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) { return apikey.Principal{}, nil }), models, map[providercredentials.ProviderID]Executor{providercredentials.Plugin: pluginprovider.New(client)}, 1<<20)
	server := httptest.NewServer(handler)
	defer server.Close()
	python := `from openai import OpenAI
c=OpenAI(api_key="service-key",base_url="` + server.URL + `/v1")
r=c.images.generate(model="example-image-v1",prompt="draw a cat",n=1,response_format="b64_json")
assert r.data[0].b64_json.startswith("iVBOR")`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/openai-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python plugin Images SDK: %v: %s", err, output)
	}
	javascript := `const OpenAI=require("openai").default;(async()=>{const c=new OpenAI({apiKey:"service-key",baseURL:"` + server.URL + `/v1"});const r=await c.images.generate({model:"example-image-v1",prompt:"draw a cat",n:1,response_format:"b64_json"});if(!r.data[0].b64_json.startsWith("iVBOR"))process.exit(2)})().catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/openai-sdk-node/node_modules")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("JavaScript plugin Images SDK: %v: %s", err, output)
	}
}
