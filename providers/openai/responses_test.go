package openai

import (
	"context"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestResponsesExecutorPinsPathAndReplacesCredential(t *testing.T) {
	registry, _ := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_OPENAI_API_KEY" {
			return "provider-secret", true
		}
		return "", false
	})
	called := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.URL.String() != "https://api.openai.com/v1/responses" || r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("Cookie") != "" {
			t.Fatalf("request=%s headers=%v", r.URL, r.Header)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	executor := NewResponsesWithClient(registry, time.Second, client)
	response, err := executor.Create(context.Background(), ResponsesRequest{ChannelID: "channel_00000000000000000000000000000001", ContentType: "application/json", Body: strings.NewReader("{}")})
	if err != nil || !called {
		t.Fatalf("called=%v err=%v", called, err)
	}
	response.Body.Close()
}
