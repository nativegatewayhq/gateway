package plugin

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/plugins"
	asyncv1 "github.com/nativegatewayhq/gateway/plugin-sdk/async/v1"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
)

type callbackStub struct {
	calls   int
	request jobs.WebhookObservation
	err     error
}

func (stub *callbackStub) ApplyPluginWebhook(_ context.Context, request jobs.WebhookObservation) (bool, error) {
	stub.calls++
	stub.request = request
	return false, stub.err
}

func callbackFixture(t *testing.T) (*CallbackHandler, *callbackStub, asyncv1.Callback, []byte) {
	t.Helper()
	raw := []byte(`{"schema_version":"nativegateway.provider/v1","id":"provider.async-example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"sidecar","auth_secret_ref":"token"},"models":[{"id":"example-async-image-v1","protocols":["replicate"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["base64"],"maximum_images":2},"async":{"contract":"async/v1","callback":true}}]}`)
	validated, err := manifest.Parse(raw, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := plugins.Config{EndpointOrigins: map[string]string{"sidecar": "http://127.0.0.1:8080"}, AuthSecrets: map[string]string{"token": "0123456789abcdef"}, Timeout: time.Second, MaximumRequestBytes: 1 << 20, MaximumResponseBytes: 2 << 20, MaximumConcurrency: 1}
	registry, err := plugins.NewRegistry([]manifest.Validated{validated}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stub := &callbackStub{}
	secret := bytes.Repeat([]byte{7}, 32)
	handler, err := NewCallbackHandler(registry, stub, [][]byte{secret}, 5*time.Minute, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return time.Unix(1700000000, 0) }
	binding := registry.Bindings()[0]
	callback := asyncv1.Callback{SchemaVersion: asyncv1.CallbackSchema, DeliveryID: "delivery_abcdefghijklmnop", RequestID: "request_1", GatewayJobID: "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PluginID: binding.PluginID, PluginVersion: binding.Version, ManifestDigest: binding.DigestHex(), Protocol: "replicate", Operation: "image.generate", Model: binding.Model, ProviderJobRef: "provider:job-1", Observation: asyncv1.Observation{Status: "CANCELED"}}
	return handler, stub, callback, secret
}

func signedCallbackRequest(t *testing.T, callback asyncv1.Callback, secret []byte) *http.Request {
	t.Helper()
	expected := asyncv1.Expectation{Identity: callback.Identity(), Output: "base64", MaximumImages: 2}
	body, err := asyncv1.CanonicalCallback(callback, expected)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := asyncv1.SignCallback(secret, 1700000000, callback.DeliveryID, body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/webhooks/plugin/"+callback.GatewayJobID+"/whk_abcdefghijklmnop", bytes.NewReader(body))
	request.Header.Set(asyncv1.CallbackTimestampHeader, "1700000000")
	request.Header.Set(asyncv1.CallbackDeliveryHeader, callback.DeliveryID)
	request.Header.Set(asyncv1.CallbackSignatureHeader, signature)
	return request
}

func TestPluginCallbackVerifiesIdentityBeforeApplying(t *testing.T) {
	handler, stub, callback, secret := callbackFixture(t)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, signedCallbackRequest(t, callback, secret))
	if response.Code != http.StatusNoContent || stub.calls != 1 || stub.request.Provider != "plugin" || stub.request.ProviderJobID != "provider:job-1" {
		t.Fatalf("status=%d calls=%d request=%#v", response.Code, stub.calls, stub.request)
	}
}

func TestPluginCallbackRejectsTamperDuplicateHeaderAndStaleTimestamp(t *testing.T) {
	for name, mutate := range map[string]func(*http.Request){"tamper": func(request *http.Request) {
		request.Header.Set(asyncv1.CallbackSignatureHeader, "v1="+fmt.Sprintf("%064d", 0))
	}, "duplicate": func(request *http.Request) {
		request.Header.Add(asyncv1.CallbackDeliveryHeader, "delivery_ponmlkjihgfedcba")
	}, "stale": func(request *http.Request) { request.Header.Set(asyncv1.CallbackTimestampHeader, "1699990000") }} {
		t.Run(name, func(t *testing.T) {
			handler, stub, callback, secret := callbackFixture(t)
			request := signedCallbackRequest(t, callback, secret)
			mutate(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code < 400 || stub.calls != 0 {
				t.Fatalf("status=%d calls=%d", response.Code, stub.calls)
			}
		})
	}
}
