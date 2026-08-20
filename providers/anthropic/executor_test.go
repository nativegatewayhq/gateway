package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

func TestCreateMessageUsesTrustedPathAndCredential(t *testing.T) {
	registry, err := providercredentials.Load(func(key string) (string, bool) { return "provider-secret", key == "GATEWAY_ANTHROPIC_API_KEY" })
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/messages" || r.URL.RawQuery != "" || r.Header.Get("x-api-key") != "provider-secret" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Fatalf("unexpected request: %s?%s %#v", r.URL.Path, r.URL.RawQuery, r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"model":"claude-test"}` {
			t.Fatalf("body = %s", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1"}`))}, nil
	})}
	origin, _ := url.Parse("https://api.anthropic.test/attacker?key=service")
	executor := newExecutor(origin, client, registry, time.Second)
	channel, _ := providercredentials.LegacyChannel(providercredentials.Anthropic)
	response, err := executor.CreateMessage(context.Background(), MessagesRequest{ChannelID: channel, Version: "2023-06-01", ContentType: "application/json", ContentLength: 23, Body: strings.NewReader(`{"model":"claude-test"}`)})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
}

func TestCreateMessageRejectsRedirect(t *testing.T) {
	registry, _ := providercredentials.Load(func(key string) (string, bool) { return "provider-secret", key == "GATEWAY_ANTHROPIC_API_KEY" })
	client := NewWithClient(registry, time.Second, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://example.com/steal"}}, Body: io.NopCloser(strings.NewReader("redirect"))}, nil
	})}).client
	origin, _ := url.Parse("https://api.anthropic.test")
	executor := newExecutor(origin, client, registry, time.Second)
	channel, _ := providercredentials.LegacyChannel(providercredentials.Anthropic)
	response, err := executor.CreateMessage(context.Background(), MessagesRequest{ChannelID: channel, Version: "2023-06-01", Body: strings.NewReader(`{}`)})
	if err != nil || response.StatusCode != http.StatusFound {
		t.Fatalf("response=%v err=%v", response, err)
	}
	_ = response.Body.Close()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
