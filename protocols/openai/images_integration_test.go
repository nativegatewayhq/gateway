//go:build integration

package openai

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	chatoperation "github.com/nativegatewayhq/gateway/operations/chat"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
	"github.com/nativegatewayhq/gateway/providers/xai"
)

type integrationRoundTripFunc func(*http.Request) (*http.Response, error)

func (function integrationRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestPostgresServiceKeyAuthenticatesOpenAIChatRoute(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	record, raw, err := apikey.Generate(rand.Reader, "openai chat integration", nil)
	if err != nil {
		t.Fatal(err)
	}
	store := apikey.NewPostgresStore(pool)
	if err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM service_api_keys WHERE id=$1`, record.ID)
	credentials, err := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_OPENAI_API_KEY" {
			return "chat-provider-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	requestBody := `{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}],"unknown_option":true}`
	nativeBody := `{"id":"chatcmpl_1","object":"chat.completion","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	calls := 0
	client := &http.Client{Transport: integrationRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(request.Body)
		if request.URL.Path != "/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer chat-provider-secret" || string(body) != requestBody || strings.Contains(string(body), raw) {
			t.Fatalf("unsafe upstream request path=%s body=%q", request.URL.Path, body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(nativeBody))}, nil
	})}
	models, _ := chatoperation.NewRegistry([]string{"gpt-4.1"})
	handler := NewChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), apikey.NewService(store), models, openaiProvider.NewChatWithClient(credentials, time.Second, client), credentials, providerhealth.NoopGate{}, 4096)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+raw)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != nativeBody || calls != 1 {
		t.Fatalf("response=%d %q calls=%d", response.Code, response.Body.String(), calls)
	}
}

func TestPostgresServiceKeyAuthenticatesOpenAIImagesRoute(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	record, raw, err := apikey.Generate(rand.Reader, "openai images integration", nil)
	if err != nil {
		t.Fatal(err)
	}
	store := apikey.NewPostgresStore(pool)
	if err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM service_api_keys WHERE id=$1`, record.ID)

	registry, err := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_OPENAI_API_KEY" {
			return "openai-integration-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	nativeBody := `{"data":[{"b64_json":"aW1hZ2U=","revised_prompt":"draw"}],"usage":{"total_tokens":10}}`
	requestBody := `{"model":"gpt-image-1","prompt":"draw"}`
	upstreamCalls := 0
	client := &http.Client{Transport: integrationRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		upstreamCalls++
		if request.URL.String() != "https://api.openai.com/v1/images/generations" {
			t.Fatalf("URL = %s", request.URL.Redacted())
		}
		if request.Header.Get("Authorization") != "Bearer openai-integration-secret" {
			t.Fatalf("provider credential missing")
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != requestBody || strings.Contains(string(body), raw) {
			t.Fatalf("request body changed or leaked service key")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(nativeBody))}, nil
	})}
	executor := openaiProvider.NewWithClient(registry, time.Second, client)
	models, _ := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "gpt-image-1", Owner: "openai", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Fixed, FixedCandidateID: "candidate_openai", Candidates: []imageoperation.ChannelCandidate{{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "gpt-image-1", ChannelID: "channel_00000000000000000000000000000001", Enabled: true}}})
	handler := NewImagesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), apikey.NewService(store), models, map[providercredentials.ProviderID]Executor{providercredentials.OpenAI: executor}, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations?key="+raw, strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != nativeBody || upstreamCalls != 1 {
		t.Fatalf("response=%d %q calls=%d", response.Code, response.Body.String(), upstreamCalls)
	}
	modelsHandler := NewModelsHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), apikey.NewService(store), models, registry)
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRequest.Header.Set("Authorization", "Bearer "+raw)
	modelsResponse := httptest.NewRecorder()
	modelsHandler.ServeHTTP(modelsResponse, modelsRequest)
	if modelsResponse.Code != 200 || !strings.Contains(modelsResponse.Body.String(), `"id":"gpt-image-1"`) {
		t.Fatalf("models response=%d %s", modelsResponse.Code, modelsResponse.Body.String())
	}
}

func TestPostgresServiceKeyAuthenticatesNativeImageEdits(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	record, raw, err := apikey.Generate(rand.Reader, "image edits integration", nil)
	if err != nil {
		t.Fatal(err)
	}
	store := apikey.NewPostgresStore(pool)
	if err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM service_api_keys WHERE id=$1`, record.ID)
	credentials, _ := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_OPENAI_API_KEY" {
			return "openai-edit-secret", true
		}
		if key == "GATEWAY_XAI_API_KEY" {
			return "xai-edit-secret", true
		}
		return "", false
	})
	models := imageoperation.DefaultRegistry()
	tests := []struct {
		name, model, contentType, body, host, credential string
		provider                                         providercredentials.ProviderID
	}{
		{name: "xai json", model: "grok-imagine-image-quality", contentType: "application/json", body: `{"model":"grok-imagine-image-quality","prompt":"edit","image":{"url":"https://example.invalid/i"}}`, host: "api.x.ai", credential: "xai-edit-secret", provider: providercredentials.XAI},
	}
	multipartBody, multipartType := multipartEdit(t, "gpt-image-1")
	tests = append(tests, struct {
		name, model, contentType, body, host, credential string
		provider                                         providercredentials.ProviderID
	}{name: "openai multipart", model: "gpt-image-1", contentType: multipartType, body: string(multipartBody), host: "api.openai.com", credential: "openai-edit-secret", provider: providercredentials.OpenAI})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: integrationRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Host != test.host || request.URL.Path != "/v1/images/edits" || request.Header.Get("Authorization") != "Bearer "+test.credential {
					t.Fatalf("unsafe request: %s %v", request.URL.Redacted(), request.Header)
				}
				got, _ := io.ReadAll(request.Body)
				if string(got) != test.body || strings.Contains(string(got), raw) {
					t.Fatal("native body changed or service key leaked")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
			})}
			var executor Executor
			if test.provider == providercredentials.OpenAI {
				executor = openaiProvider.NewWithClient(credentials, time.Second, client)
			} else {
				executor = xai.NewWithClient(credentials, time.Second, client)
			}
			handler := NewEditHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), apikey.NewService(store), models, map[providercredentials.ProviderID]Executor{test.provider: executor}, 1024*1024, 1)
			request := httptest.NewRequest(http.MethodPost, "/v1/images/edits?key="+raw, strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 200 || calls != 1 {
				t.Fatalf("response=%d %s calls=%d", response.Code, response.Body.String(), calls)
			}
		})
	}
}
