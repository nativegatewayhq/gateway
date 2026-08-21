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

type speechRoundTripper func(*http.Request) (*http.Response, error)

func (function speechRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestSpeechUsesFixedWireAndProviderCredential(t *testing.T) {
	credentials, _ := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_OPENAI_API_KEY" {
			return "provider-secret", true
		}
		return "", false
	})
	client := &http.Client{Transport: speechRoundTripper(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		if request.URL.String() != "https://api.openai.com/v1/audio/speech" || request.Header.Get("Authorization") != "Bearer provider-secret" || request.Header.Get("Cookie") != "" || string(body) != `{"model":"tts-1"}` {
			t.Fatalf("url=%s headers=%v body=%s", request.URL, request.Header, body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(strings.NewReader("audio"))}, nil
	})}
	executor := NewSpeechWithClient(credentials, time.Second, time.Second, client)
	response, err := executor.Create(context.Background(), SpeechRequest{ChannelID: "channel_00000000000000000000000000000001", ContentType: "application/json", ContentLength: 17, Body: strings.NewReader(`{"model":"tts-1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
}
