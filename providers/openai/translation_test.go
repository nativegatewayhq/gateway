package openai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

func TestTranslationUsesFixedOriginAndReplacesCredential(t *testing.T) {
	registry, _ := providercredentials.Load(func(key string) (string, bool) {
		return "provider-secret", key == "GATEWAY_OPENAI_API_KEY"
	})
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		if request.URL.String() != "https://api.openai.com/v1/audio/translations" || request.Header.Get("Authorization") != "Bearer provider-secret" || request.Header.Get("OpenAI-Organization") != "" || string(body) != "multipart" {
			t.Fatalf("url=%s headers=%v body=%s", request.URL, request.Header, body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"text":"english"}`))}, nil
	})}
	executor := NewTranslationWithClient(registry, time.Second, client)
	response, err := executor.Create(context.Background(), TranslationRequest{ChannelID: "channel_00000000000000000000000000000001", ContentType: "multipart/form-data; boundary=x", ContentLength: 9, Body: strings.NewReader("multipart")})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("response=%v err=%v", response, err)
	}
	response.Body.Close()
}
