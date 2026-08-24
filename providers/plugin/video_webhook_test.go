package plugin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/plugins"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	videov1 "github.com/nativegatewayhq/gateway/plugin-sdk/video/v1"
)

func TestVideoCallbackIsModalityBoundAndProducesCreditEvidence(t *testing.T) {
	validated, err := manifest.Parse([]byte(`{"schema_version":"nativegateway.provider/v1","id":"provider.video-example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"sidecar","auth_secret_ref":"token"},"models":[],"video_models":[{"id":"example-video-v1","protocols":["runway"],"operations":["video.generate"],"capabilities":{"media_type":"application/json","output":["url"],"text_to_video":true,"image_to_video":false,"audio":false,"maximum_duration_seconds":60,"ratios":["16:9"]},"async":{"contract":"video/v1","callback":true}}]}`), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := plugins.NewRegistry([]manifest.Validated{validated}, plugins.Config{EndpointOrigins: map[string]string{"sidecar": "http://127.0.0.1:8080"}, AuthSecrets: map[string]string{"token": "0123456789abcdef"}, ResultOrigins: map[string][]string{"provider.video-example": {"https://assets.example.com"}}, Timeout: time.Second, MaximumRequestBytes: 1 << 20, MaximumResponseBytes: 2 << 20, MaximumConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	stub := &callbackStub{}
	secret := bytes.Repeat([]byte{9}, 32)
	handler, err := NewVideoCallbackHandler(registry, stub, [][]byte{secret}, 5*time.Minute, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return time.Unix(1700000000, 0) }
	binding := registry.Bindings()[0]
	identity := videov1.Identity{RequestID: "request_1", GatewayJobID: "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PluginID: binding.PluginID, PluginVersion: binding.Version, ManifestDigest: binding.DigestHex()}
	expected := videoExpectationForCallback(binding, identity)
	callback := videov1.Callback{SchemaVersion: videov1.CallbackSchema, DeliveryID: "delivery_abcdefghijklmnop", RequestID: identity.RequestID, GatewayJobID: identity.GatewayJobID, PluginID: identity.PluginID, PluginVersion: identity.PluginVersion, ManifestDigest: identity.ManifestDigest, Protocol: "runway", Operation: "video.generate", Model: binding.Model, ProviderJobRef: "provider:video-1", Observation: videov1.Observation{Status: "SUCCEEDED", Result: &videov1.Result{URL: "https://assets.example.com/result.mp4", ContentType: "video/mp4", DurationSeconds: 5}, Usage: &videov1.Usage{Dimension: "provider_credit", Unit: "microcredit", Quantity: 750000}}}
	body, _ := videov1.CanonicalCallback(callback, expected)
	signature, _ := videov1.SignCallback(secret, 1700000000, callback.DeliveryID, body)
	request := httptest.NewRequest(http.MethodPost, "/internal/webhooks/plugin-video/"+identity.GatewayJobID+"/whk_abcdefghijklmnop", bytes.NewReader(body))
	request.Header.Set(videov1.CallbackTimestampHeader, "1700000000")
	request.Header.Set(videov1.CallbackDeliveryHeader, callback.DeliveryID)
	request.Header.Set(videov1.CallbackSignatureHeader, signature)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || stub.calls != 1 || stub.request.Observation.Usage == nil || stub.request.Observation.Usage.Quantity != 750000 || stub.request.Observation.Usage.ExtractorVersion != "runway-task-cost-v1" {
		t.Fatalf("status=%d calls=%d request=%#v", response.Code, stub.calls, stub.request)
	}
}
