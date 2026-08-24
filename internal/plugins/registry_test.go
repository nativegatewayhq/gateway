package plugins

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	registryv1 "github.com/nativegatewayhq/gateway/plugin-sdk/registry/v1"
)

func validated(t *testing.T, id, protocol string) manifest.Validated {
	t.Helper()
	body := []byte(`{"schema_version":"nativegateway.provider/v1","id":"` + id + `","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"sidecar","auth_secret_ref":"token"},"models":[{"id":"example-image-v1","protocols":["` + protocol + `"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["base64"],"maximum_images":2}}]}`)
	value, err := manifest.Parse(body, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func validatedAsync(t *testing.T, id, protocol string, callback bool) manifest.Validated {
	t.Helper()
	body := []byte(`{"schema_version":"nativegateway.provider/v1","id":"` + id + `","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"sidecar","auth_secret_ref":"token"},"models":[{"id":"example-async-image-v1","protocols":["` + protocol + `"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["url"],"maximum_images":2},"async":{"contract":"async/v1","callback":` + strconv.FormatBool(callback) + `}}]}`)
	value, err := manifest.Parse(body, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func testConfig() Config {
	return Config{EndpointOrigins: map[string]string{"sidecar": "http://127.0.0.1:8080"}, AuthSecrets: map[string]string{"token": "secret"}, Timeout: time.Second, MaximumRequestBytes: 1 << 20, MaximumResponseBytes: 2 << 20, MaximumConcurrency: 2}
}

func TestRegistryProducesStableImmutableRoute(t *testing.T) {
	registry, err := NewRegistry([]manifest.Validated{validated(t, "provider.example", "openai")}, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	routes := registry.Routes()
	if len(routes) != 1 || routes[0].Owner != "provider.example" || len(routes[0].Candidates) != 1 {
		t.Fatalf("unexpected routes: %#v", routes)
	}
	binding, ok := registry.Binding(routes[0].Candidates[0].ChannelID)
	if !ok || binding.PluginID != "provider.example" || binding.DigestHex() == "" {
		t.Fatalf("unexpected binding: %#v", binding)
	}
	routes[0].Owner = "changed"
	if registry.Routes()[0].Owner != "provider.example" {
		t.Fatal("registry route mutated")
	}
}

func TestRegistryPublishesAsyncBindingWithoutSyncExecution(t *testing.T) {
	cfg := testConfig()
	cfg.ResultOrigins = map[string][]string{"provider.async-example": {"https://assets.example.com"}}
	registry, err := NewRegistry([]manifest.Validated{validatedAsync(t, "provider.async-example", "replicate", true)}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	binding := registry.Bindings()[0]
	if !binding.Async || !binding.Callback || !registry.SupportsAsyncProtocol("replicate") || registry.SupportsAsyncProtocol("openai") {
		t.Fatalf("async binding = %#v", binding)
	}
	client := NewClient(registry)
	if _, err = client.Execute(context.Background(), binding.ChannelID, "request", "replicate", ImageInput{Prompt: "x", Images: 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("sync execution accepted async binding: %v", err)
	}
}

func TestRegistryRejectsUnsafeOriginAndCollision(t *testing.T) {
	cfg := testConfig()
	cfg.EndpointOrigins["sidecar"] = "http://example.com"
	if _, err := NewRegistry([]manifest.Validated{validated(t, "provider.example", "openai")}, cfg); err == nil {
		t.Fatal("expected unsafe origin rejection")
	}
	if _, err := NewRegistry([]manifest.Validated{validated(t, "provider.one", "openai"), validated(t, "provider.two", "openai")}, testConfig()); err == nil {
		t.Fatal("expected model collision rejection")
	}
}

func TestAdmittedRegistryChangesChannelAndCarriesImmutableEvidence(t *testing.T) {
	item := validated(t, "provider.example", "openai")
	plain, err := NewRegistry([]manifest.Validated{item}, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	admissionDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	snapshot := registryv1.Snapshot{Index: registryv1.VerifiedIndex{Index: registryv1.Index{Sequence: 7, CreatedAt: time.Now().UTC().Truncate(time.Second), ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Second)}, EnvelopeDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", PayloadDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}, Admissions: map[string]registryv1.VerifiedAdmission{"provider.example@1.0.0": {EnvelopeDigest: admissionDigest}}}
	admitted, err := NewAdmittedRegistry([]manifest.Validated{item}, testConfig(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	binding := admitted.Bindings()[0]
	if binding.ChannelID == plain.Bindings()[0].ChannelID || binding.RegistrySequence != 7 || binding.RegistryIndexDigest == [32]byte{} || binding.AdmissionDigest == [32]byte{} {
		t.Fatalf("missing admitted identity: %#v", binding)
	}
}
