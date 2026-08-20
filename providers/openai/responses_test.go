package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
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

type blockingReadCloser struct{ closed chan struct{} }

func (b *blockingReadCloser) Read([]byte) (int, error) { <-b.closed; return 0, io.EOF }
func (b *blockingReadCloser) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestResponsesStreamingBodyEnforcesIdleTimeout(t *testing.T) {
	registry, _ := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_OPENAI_API_KEY" {
			return "provider-secret", true
		}
		return "", false
	})
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &blockingReadCloser{closed: make(chan struct{})}}, nil
	})}
	executor := NewResponsesWithClient(registry, time.Second, client)
	executor.streamIdleTimeout = time.Millisecond
	response, err := executor.Create(context.Background(), ResponsesRequest{ChannelID: "channel_00000000000000000000000000000001", ContentType: "application/json", Body: strings.NewReader("{}"), Streaming: true})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err = response.Body.Read(make([]byte, 1)); !errors.Is(err, ErrResponsesStreamIdle) {
		t.Fatalf("err=%v", err)
	}
}
