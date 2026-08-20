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

type chatRoundTripFunc func(*http.Request) (*http.Response, error)

func (f chatRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestChatPinsXAIOriginAndCredentialScope(t *testing.T) {
	registry, err := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_XAI_API_KEY" {
			return "xai-provider-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: chatRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://api.x.ai/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer xai-provider-secret" {
			t.Fatalf("url=%s headers=%v", request.URL, request.Header)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	executor := NewChatWithClient(registry, time.Second, client)
	response, err := executor.Complete(context.Background(), openaiProvider.ChatRequest{ChannelID: "channel_00000000000000000000000000000002", ContentType: "application/json", Body: strings.NewReader("{}")})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
}
