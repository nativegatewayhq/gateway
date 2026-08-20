package xai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type responsesRoundTrip func(*http.Request) (*http.Response, error)

func (f responsesRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResponsesUsesFixedXAIOriginAndCredential(t *testing.T) {
	registry, err := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_XAI_API_KEY" {
			return "xai-test-token", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: responsesRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.x.ai/v1/responses" || r.Header.Get("Authorization") != "Bearer xai-test-token" {
			t.Fatalf("url=%s auth=%q", r.URL, r.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp"}`))}, nil
	})}
	executor := NewResponsesWithClient(registry, time.Second, client)
	response, err := executor.Create(context.Background(), openaiProvider.ResponsesRequest{ChannelID: "channel_00000000000000000000000000000002", ContentType: "application/json", Body: strings.NewReader(`{}`), ContentLength: 2})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}
