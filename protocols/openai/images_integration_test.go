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
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type integrationRoundTripFunc func(*http.Request) (*http.Response, error)

func (function integrationRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
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
	models, _ := imageoperation.NewRegistry(imageoperation.ModelRoute{Model: "gpt-image-1", Provider: providercredentials.OpenAI})
	handler := NewImagesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), apikey.NewService(store), models, map[providercredentials.ProviderID]Executor{providercredentials.OpenAI: executor}, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations?key="+raw, strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != nativeBody || upstreamCalls != 1 {
		t.Fatalf("response=%d %q calls=%d", response.Code, response.Body.String(), upstreamCalls)
	}
}
