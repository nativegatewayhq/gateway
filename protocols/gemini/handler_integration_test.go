//go:build integration

package gemini

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/providers/google"
)

func TestPostgresServiceKeyAuthenticatesGeminiRoute(t *testing.T) {
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
	record, raw, err := apikey.Generate(rand.Reader, "gemini integration", nil)
	if err != nil {
		t.Fatal(err)
	}
	store := apikey.NewPostgresStore(pool)
	if err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM service_api_keys WHERE id=$1`, record.ID)

	nativeBody := `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}}]}`
	registry, err := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_GOOGLE_API_KEY" {
			return "google-integration-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	var upstreamCalls int
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		upstreamCalls++
		if request.URL.Scheme != "https" || request.URL.Host != "generativelanguage.googleapis.com" {
			t.Errorf("untrusted upstream URL: %s", request.URL.Redacted())
		}
		if request.Header.Get("x-goog-api-key") != "google-integration-secret" || request.URL.Query().Get("key") != "" {
			t.Errorf("credential boundary failed: headers=%v query=%v", request.Header, request.URL.Query())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(nativeBody)),
			Request:    request,
		}, nil
	})}
	executor := google.NewWithClient(registry, time.Second, client)
	handler := NewHandler(testLogger(io.Discard), apikey.NewService(store), executor, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-image:generateContent?key="+raw, bytes.NewBufferString(`{"contents":"draw"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != nativeBody || upstreamCalls != 1 {
		t.Fatalf("response=%d %q calls=%d", response.Code, response.Body.String(), upstreamCalls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
