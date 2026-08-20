package anthropic

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	operation "github.com/nativegatewayhq/gateway/operations/anthropic"
	provider "github.com/nativegatewayhq/gateway/providers/anthropic"
)

type authStub struct{}

func (authStub) Authenticate(context.Context, string) (apikey.Principal, error) {
	return apikey.Principal{}, nil
}

type availableStub struct{}

func (availableStub) ConfiguredChannel(context.Context, string, providercredentials.ProviderID) bool {
	return true
}

type executorStub struct {
	called  bool
	request provider.MessagesRequest
}

func (stub *executorStub) CreateMessage(_ context.Context, request provider.MessagesRequest) (*http.Response, error) {
	stub.called = true
	stub.request = request
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "Request-Id": {"req_provider"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message"}`))}, nil
}

func testHandler(t *testing.T, executor *executorStub, billing bool) *Handler {
	t.Helper()
	registry, err := operation.NewRegistry([]string{"claude-test"})
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), authStub{}, registry, executor, availableStub{}, nil, 4096, billing)
}
func request(body string) *http.Request {
	value := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	value.Header.Set("x-api-key", "service-secret")
	value.Header.Set("anthropic-version", "2023-06-01")
	value.Header.Set("Content-Type", "application/json")
	return value
}

func TestMessagesNativePassThrough(t *testing.T) {
	executor := &executorStub{}
	recorder := httptest.NewRecorder()
	testHandler(t, executor, false).ServeHTTP(recorder, request(`{"model":"claude-test","max_tokens":16,"messages":[]}`))
	if recorder.Code != 200 || !executor.called || recorder.Header().Get("Request-Id") != "req_provider" || recorder.Body.String() != `{"id":"msg_1","type":"message"}` {
		t.Fatalf("code=%d called=%v headers=%v body=%s", recorder.Code, executor.called, recorder.Header(), recorder.Body.String())
	}
	if executor.request.Version != "2023-06-01" {
		t.Fatalf("version=%q", executor.request.Version)
	}
}

func TestBillingRequiredFailsBeforeReadingOrDispatch(t *testing.T) {
	executor := &executorStub{}
	recorder := httptest.NewRecorder()
	value := request(`{"model":"claude-test"}`)
	value.Body = panicReader{}
	testHandler(t, executor, true).ServeHTTP(recorder, value)
	if recorder.Code != 503 || executor.called {
		t.Fatalf("code=%d called=%v", recorder.Code, executor.called)
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("body was read") }
func (panicReader) Close() error             { return nil }

func TestMessagesRejectsStreamingDuplicateModelAndUnsafeHeaders(t *testing.T) {
	tests := []struct {
		name, body string
		mutate     func(*http.Request)
	}{{"stream", `{"model":"claude-test","stream":true}`, nil}, {"duplicate", `{"model":"claude-test","model":"claude-test"}`, nil}, {"missing-version", `{"model":"claude-test"}`, func(r *http.Request) { r.Header.Del("anthropic-version") }}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &executorStub{}
			recorder := httptest.NewRecorder()
			value := request(test.body)
			if test.mutate != nil {
				test.mutate(value)
			}
			testHandler(t, executor, false).ServeHTTP(recorder, value)
			if recorder.Code != 400 || executor.called {
				t.Fatalf("code=%d called=%v", recorder.Code, executor.called)
			}
		})
	}
}
