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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestChatExecutorPinsPathAndReplacesCredential(t *testing.T) {
	registry, err := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_OPENAI_API_KEY" {
			return "provider-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.URL.String() != "https://api.openai.com/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("Cookie") != "" {
			t.Fatalf("request=%s headers=%v", r.URL, r.Header)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	executor := NewChatWithClient(registry, time.Second, client)
	response, err := executor.Complete(context.Background(), ChatRequest{ChannelID: "channel_00000000000000000000000000000001", ContentType: "application/json", Body: strings.NewReader("{}")})
	if err != nil || !called {
		t.Fatalf("called=%v err=%v", called, err)
	}
	response.Body.Close()
}

func TestChatExecutorPropagatesCancellation(t *testing.T) {
	registry, _ := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_OPENAI_API_KEY" {
			return "provider-secret", true
		}
		return "", false
	})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
	executor := NewChatWithClient(registry, time.Minute, client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := executor.Complete(ctx, ChatRequest{ChannelID: "channel_00000000000000000000000000000001", Body: strings.NewReader("{}")}); err != ErrChatCanceled {
		t.Fatalf("err=%v", err)
	}
}
