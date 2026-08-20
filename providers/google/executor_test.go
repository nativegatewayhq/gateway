package google

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

func googleRegistry(t *testing.T, configured bool) *providercredentials.Registry {
	t.Helper()
	registry, err := providercredentials.Load(func(key string) (string, bool) {
		if configured && key == "GATEWAY_GOOGLE_API_KEY" {
			return "google-provider-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func executorForServer(t *testing.T, server *httptest.Server, timeout time.Duration, registry *providercredentials.Registry) *Executor {
	t.Helper()
	origin, err := url.Parse(server.URL + "/proxy-prefix")
	if err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	client.CheckRedirect = rejectRedirect
	return newExecutor(origin, client, registry, timeout)
}

func TestGenerateContentBuildsTrustedRequest(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/proxy-prefix/v1beta/models/gemini-image:generateContent" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.URL.Query().Get("safe") != "value" || request.URL.Query().Get("key") != "" || request.URL.Query().Get("TOKEN") != "" {
			t.Errorf("query = %q", request.URL.RawQuery)
		}
		if got := request.Header.Get("x-goog-api-key"); got != "google-provider-secret" {
			t.Errorf("google credential = %q", got)
		}
		if got := request.Header.Get("Authorization"); got != "" {
			t.Errorf("authorization leaked = %q", got)
		}
		if request.Header.Get("x-goog-api-client") != "google-genai-sdk/test" {
			t.Errorf("API client header = %q", request.Header.Get("x-goog-api-client"))
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != "{\"contents\":[{\"parts\":[{\"text\":\"hello\"}]}]}" {
			t.Errorf("body = %q", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	executor := executorForServer(t, server, time.Second, googleRegistry(t, true))
	response, err := executor.GenerateContent(context.Background(), GenerateContentRequest{
		Model:       "gemini-image",
		Query:       url.Values{"safe": {"value"}, "key": {"service-secret"}, "TOKEN": {"other-secret"}},
		ContentType: "application/json; charset=utf-8",
		Accept:      "application/json",
		UserAgent:   "google-genai-test",
		APIClient:   "google-genai-sdk/test",
		Body:        strings.NewReader(`{"contents":[{"parts":[{"text":"hello"}]}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusCreated || string(body) != `{"ok":true}` || calls.Load() != 1 {
		t.Fatalf("response = %d %q calls=%d", response.StatusCode, body, calls.Load())
	}
}

func TestGenerateContentDoesNotFollowRedirect(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.URL.Path == "/destination" {
			t.Fatal("redirect destination was called")
		}
		http.Redirect(writer, request, "/destination", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	executor := executorForServer(t, server, time.Second, googleRegistry(t, true))
	response, err := executor.GenerateContent(context.Background(), GenerateContentRequest{Model: "model", Body: strings.NewReader(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d", response.StatusCode, calls.Load())
	}
}

func TestGenerateContentTimeoutAndCancellation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(100 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	executor := executorForServer(t, server, 20*time.Millisecond, googleRegistry(t, true))
	if _, err := executor.GenerateContent(context.Background(), GenerateContentRequest{Model: "model", Body: strings.NewReader(`{}`)}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	executor = executorForServer(t, server, time.Second, googleRegistry(t, true))
	if _, err := executor.GenerateContent(ctx, GenerateContentRequest{Model: "model", Body: strings.NewReader(`{}`)}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestGenerateContentMissingCredentialDoesNotCallUpstream(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	executor := executorForServer(t, server, time.Second, googleRegistry(t, false))
	if _, err := executor.GenerateContent(context.Background(), GenerateContentRequest{Model: "model", Body: strings.NewReader(`{}`)}); !errors.Is(err, providercredentials.ErrCredentialUnavailable) {
		t.Fatalf("credential error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestNewUsesFixedTrustedOrigin(t *testing.T) {
	t.Parallel()
	executor := New(googleRegistry(t, true), time.Second)
	if executor.origin.Scheme != "https" || executor.origin.Host != "generativelanguage.googleapis.com" || executor.origin.User != nil {
		t.Fatalf("origin = %s", executor.origin.Redacted())
	}
}

func TestGenerateContentRejectsUnsafeModelBeforeNetwork(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	executor := executorForServer(t, server, time.Second, googleRegistry(t, true))
	if _, err := executor.GenerateContent(context.Background(), GenerateContentRequest{Model: "../other", Body: strings.NewReader(`{}`)}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("model error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestGenerateContentConnectionFailureIsSingleAttempt(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("connection reset with provider-secret")
	})}
	executor := NewWithClient(googleRegistry(t, true), time.Second, client)
	if _, err := executor.GenerateContent(context.Background(), GenerateContentRequest{Model: "model", Body: strings.NewReader(`{}`)}); !errors.Is(err, ErrUpstream) || strings.Contains(err.Error(), "provider-secret") {
		t.Fatalf("connection error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("attempts = %d", calls.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
