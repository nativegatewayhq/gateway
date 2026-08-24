package plugins

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
)

// newTestClient is intentionally local so tests exercise manifest parsing,
// reference binding, fixed origin construction, and transport together.
func newTestClient(t *testing.T, handler http.Handler, maximumResponse int64) (*Client, Binding) {
	t.Helper()
	cfg := testConfig()
	cfg.MaximumResponseBytes = maximumResponse
	registry, err := NewRegistry([]manifest.Validated{validated(t, "provider.example", "openai")}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Result(), nil
	}), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{registry: registry, http: httpClient, semaphore: make(chan struct{}, cfg.MaximumConcurrency)}, registry.Bindings()[0]
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestClientSendsCanonicalIdentityAndOnlySidecarCredential(t *testing.T) {
	var calls atomic.Int64
	var binding Binding
	client, binding := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.URL.Path != "/plugin/v1/execute" || request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("Cookie") != "" || request.Header.Get("X-Forwarded-For") != "" {
			t.Error("unsafe outbound request")
		}
		var envelope ExecuteRequest
		if json.NewDecoder(request.Body).Decode(&envelope) != nil || envelope.PluginID != "provider.example" || envelope.RequestID != "req_test" || envelope.Input.Prompt != "draw" {
			t.Error("invalid envelope")
		}
		_ = json.NewEncoder(writer).Encode(ExecuteResponse{SchemaVersion: ResponseSchema, RequestID: envelope.RequestID, PluginID: envelope.PluginID, PluginVersion: envelope.PluginVersion, ManifestDigest: envelope.ManifestDigest, Result: &Result{Images: []Image{{MIMEType: "image/png", Base64: base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})}}, Usage: Usage{Images: 1}}})
	}), 1<<20)
	result, err := client.Execute(context.Background(), binding.ChannelID, "req_test", "openai", ImageInput{Prompt: "draw", Images: 1})
	if err != nil || result.Result == nil || calls.Load() != 1 {
		t.Fatalf("execute failed: %#v %v", result, err)
	}
}

func TestClientRejectsRedirectOversizeDuplicateAndIdentityMismatch(t *testing.T) {
	tests := []struct {
		name    string
		maximum int64
		handler http.HandlerFunc
	}{
		{"redirect", 1024, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) }},
		{"oversize", 8, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("123456789")) }},
		{"duplicate", 1024, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"schema_version":"x","schema_version":"y"}`))
		}},
		{"identity", 4096, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(ExecuteResponse{SchemaVersion: ResponseSchema, RequestID: "wrong", PluginID: "provider.example", PluginVersion: "1.0.0", ManifestDigest: "wrong", Error: &PluginError{Category: "internal", Message: "failed"}})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, binding := newTestClient(t, test.handler, test.maximum)
			if _, err := client.Execute(context.Background(), binding.ChannelID, "req_test", "openai", ImageInput{Prompt: "draw", Images: 1}); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestClientTimeoutAndHealth(t *testing.T) {
	client, binding := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plugin/v1/health" {
			w.WriteHeader(204)
			return
		}
		<-r.Context().Done()
	}), 1024)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.Execute(ctx, binding.ChannelID, "req_test", "openai", ImageInput{Prompt: "draw", Images: 1}); err != ErrTimeout {
		t.Fatalf("expected timeout, got %v", err)
	}
	if err := client.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClientConcurrencyCapRejectsBeforeSecondDispatch(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	var client *Client
	var binding Binding
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		close(started)
		<-release
		var envelope ExecuteRequest
		_ = json.NewDecoder(request.Body).Decode(&envelope)
		_ = json.NewEncoder(writer).Encode(ExecuteResponse{SchemaVersion: ResponseSchema, RequestID: envelope.RequestID, PluginID: envelope.PluginID, PluginVersion: envelope.PluginVersion, ManifestDigest: envelope.ManifestDigest, Result: &Result{Images: []Image{{MIMEType: "image/png", Base64: base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})}}, Usage: Usage{Images: 1}}})
	})
	client, binding = newTestClient(t, handler, 1<<20)
	client.semaphore = make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := client.Execute(context.Background(), binding.ChannelID, "req_first", "openai", ImageInput{Prompt: "draw", Images: 1})
		done <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.Execute(ctx, binding.ChannelID, "req_second", "openai", ImageInput{Prompt: "draw", Images: 1}); err != ErrTimeout {
		t.Fatalf("second error=%v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("sidecar calls=%d", calls.Load())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestClientAcceptsBoundedPluginErrorAndRejectsNon2xx(t *testing.T) {
	client, binding := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var envelope ExecuteRequest
		_ = json.NewDecoder(request.Body).Decode(&envelope)
		_ = json.NewEncoder(writer).Encode(ExecuteResponse{SchemaVersion: ResponseSchema, RequestID: envelope.RequestID, PluginID: envelope.PluginID, PluginVersion: envelope.PluginVersion, ManifestDigest: envelope.ManifestDigest, Error: &PluginError{Category: "rate_limited", Message: "try later", Retryable: true}})
	}), 4096)
	response, err := client.Execute(context.Background(), binding.ChannelID, "req_error", "openai", ImageInput{Prompt: "draw", Images: 1})
	if err != nil || response.Error == nil || response.Error.Category != "rate_limited" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	client, binding = newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusUnauthorized) }), 4096)
	if _, err = client.Execute(context.Background(), binding.ChannelID, "req_401", "openai", ImageInput{Prompt: "draw", Images: 1}); err != ErrUnavailable {
		t.Fatalf("non-2xx error=%v", err)
	}
}

func TestClientURLResultRequiresExactPluginOrigin(t *testing.T) {
	body := []byte(`{"schema_version":"nativegateway.provider/v1","id":"provider.url","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"sidecar","auth_secret_ref":"token"},"models":[{"id":"url-image-v1","protocols":["openai"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["url"],"maximum_images":1}}]}`)
	validatedManifest, err := manifest.Parse(body, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.ResultOrigins = map[string][]string{"provider.url": {"https://assets.example.com"}}
	registry, err := NewRegistry([]manifest.Validated{validatedManifest}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	binding := registry.Bindings()[0]
	resultURL := "https://assets.example.com/image.png?signature=opaque"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var envelope ExecuteRequest
		_ = json.NewDecoder(request.Body).Decode(&envelope)
		responseBody, _ := json.Marshal(ExecuteResponse{SchemaVersion: ResponseSchema, RequestID: envelope.RequestID, PluginID: envelope.PluginID, PluginVersion: envelope.PluginVersion, ManifestDigest: envelope.ManifestDigest, Result: &Result{Images: []Image{{MIMEType: "image/png", URL: resultURL}}, Usage: Usage{Images: 1}}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(responseBody)), Header: http.Header{"Content-Type": {"application/json"}}}, nil
	})
	client := NewClientWithHTTPClient(registry, &http.Client{Transport: transport})
	if _, err = client.Execute(context.Background(), binding.ChannelID, "req_url", "openai", ImageInput{Prompt: "draw", Images: 1}); err != nil {
		t.Fatal(err)
	}
	resultURL = "https://evil.example/image.png"
	if _, err = client.Execute(context.Background(), binding.ChannelID, "req_evil", "openai", ImageInput{Prompt: "draw", Images: 1}); err != ErrInvalidResponse {
		t.Fatalf("cross-origin error=%v", err)
	}
}
