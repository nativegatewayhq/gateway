package openaiimages

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func credentialRegistry(t *testing.T) *providercredentials.Registry {
	t.Helper()
	registry, err := providercredentials.Load(func(key string) (string, bool) {
		switch key {
		case "GATEWAY_OPENAI_API_KEY":
			return "openai-provider-secret", true
		case "GATEWAY_XAI_API_KEY":
			return "xai-provider-secret", true
		default:
			return "", false
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestGenerateUsesFixedOriginAndScopedCredential(t *testing.T) {
	t.Parallel()
	tests := []struct {
		provider providercredentials.ProviderID
		origin   string
		secret   string
	}{
		{providercredentials.OpenAI, "https://api.openai.com", "openai-provider-secret"},
		{providercredentials.XAI, "https://api.x.ai", "xai-provider-secret"},
	}
	for _, test := range tests {
		t.Run(string(test.provider), func(t *testing.T) {
			var attempts atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempts.Add(1)
				if request.URL.String() != test.origin+"/v1/images/generations" {
					t.Fatalf("URL = %s", request.URL)
				}
				if request.Header.Get("Authorization") != "Bearer "+test.secret {
					t.Fatalf("Authorization = %q", request.Header.Get("Authorization"))
				}
				if request.URL.RawQuery != "" || request.Host != "" || request.RequestURI != "" {
					t.Fatalf("unsafe outbound request: %+v", request)
				}
				body, _ := io.ReadAll(request.Body)
				if string(body) != `{"model":"model","prompt":"secret prompt"}` {
					t.Fatalf("body = %s", body)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
			})}
			executor := NewWithClient(test.provider, credentialRegistry(t), time.Second, client)
			response, err := executor.Generate(context.Background(), Request{ContentType: "application/json", Body: strings.NewReader(`{"model":"model","prompt":"secret prompt"}`)})
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if attempts.Load() != 1 {
				t.Fatalf("attempts = %d", attempts.Load())
			}
		})
	}
}

func TestGenerateClassifiesTimeoutCancellationAndConnectionError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ctx  func() context.Context
		err  error
		want error
	}{
		{name: "timeout", ctx: context.Background, err: context.DeadlineExceeded, want: ErrTimeout},
		{name: "connection", ctx: context.Background, err: errors.New("connection reset"), want: ErrUpstream},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, test.err })}
			executor := NewWithClient(providercredentials.OpenAI, credentialRegistry(t), time.Second, client)
			_, err := executor.Generate(test.ctx(), Request{Body: strings.NewReader(`{}`)})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, request.Context().Err()
	})}
	executor := NewWithClient(providercredentials.XAI, credentialRegistry(t), time.Second, client)
	_, err := executor.Generate(ctx, Request{Body: strings.NewReader(`{}`)})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestGenerateRejectsRedirectAndMissingCredential(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Location", "https://attacker.invalid/capture")
		return &http.Response{StatusCode: 307, Header: header, Body: io.NopCloser(strings.NewReader("redirect"))}, nil
	})}
	executor := NewWithClient(providercredentials.OpenAI, credentialRegistry(t), time.Second, client)
	response, err := executor.Generate(context.Background(), Request{Body: strings.NewReader(`{}`)})
	if err != nil || response.StatusCode != 307 {
		t.Fatalf("redirect = %v, %v", response, err)
	}
	response.Body.Close()

	empty, _ := providercredentials.Load(func(string) (string, bool) { return "", false })
	executor = NewWithClient(providercredentials.OpenAI, empty, time.Second, client)
	if _, err := executor.Generate(context.Background(), Request{Body: strings.NewReader(`{}`)}); !errors.Is(err, providercredentials.ErrCredentialUnavailable) {
		t.Fatalf("missing credential error = %v", err)
	}
}
