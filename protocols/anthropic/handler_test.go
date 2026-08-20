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
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/chatbilling"
	"github.com/nativegatewayhq/gateway/internal/chatpricing"
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

type billingStub struct {
	begin    chatbilling.BeginRequest
	usage    chatpricing.Usage
	complete int
}

func (stub *billingStub) Begin(_ context.Context, request chatbilling.BeginRequest) (chatbilling.Charge, error) {
	stub.begin = request
	return chatbilling.Charge{ID: "chc_00000000000000000000000000000001", MaximumInputTokens: request.MaximumInputTokens, MaximumOutputTokens: request.MaximumOutputTokens}, nil
}
func (*billingStub) Replay(context.Context, chatbilling.BeginRequest) (chatbilling.Charge, bool, error) {
	return chatbilling.Charge{}, false, nil
}
func (stub *billingStub) CompleteUsage(_ context.Context, _ string, usage chatpricing.Usage, snapshot billing.ResponseSnapshot) (chatbilling.Charge, error) {
	stub.complete++
	stub.usage = usage
	return chatbilling.Charge{Response: snapshot}, nil
}
func (*billingStub) Release(_ context.Context, _ string, snapshot billing.ResponseSnapshot) (chatbilling.Charge, error) {
	return chatbilling.Charge{Response: snapshot}, nil
}
func (*billingStub) MarkReconciling(context.Context, string, string, *billing.ResponseSnapshot) error {
	return nil
}
func (*billingStub) MarkReconcilingUsage(context.Context, string, string, *billing.ResponseSnapshot, chatpricing.Usage) error {
	return nil
}

type managedExecutor struct{ called bool }

func (stub *managedExecutor) CreateMessage(context.Context, provider.MessagesRequest) (*http.Response, error) {
	stub.called = true
	body := `{"id":"msg_paid","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":4}}`
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestManagedMessagesReservesThenSettlesFourUsageAxes(t *testing.T) {
	models, _ := operation.NewRegistryWithLimits([]string{"claude-test"}, map[string]operation.Limits{"claude-test": {MaximumInputTokens: 4096, MaximumOutputTokens: 100}})
	executor := &managedExecutor{}
	charges := &billingStub{}
	handler := NewBillableHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), authStub{}, models, executor, availableStub{}, nil, 4096, charges)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request(`{"model":"claude-test","max_tokens":16,"messages":[]}`))
	if recorder.Code != 200 || !executor.called || charges.complete != 1 || charges.begin.Protocol != "anthropic" || charges.begin.Operation != operation.CreateMessage || charges.begin.MaximumOutputTokens != 16 {
		t.Fatalf("code=%d called=%v begin=%+v complete=%d", recorder.Code, executor.called, charges.begin, charges.complete)
	}
	want := chatpricing.Usage{PromptTokens: 10, CachedInputTokens: 3, CacheWriteTokens: 2, CompletionTokens: 4}
	if charges.usage != want {
		t.Fatalf("usage=%+v", charges.usage)
	}
}

func TestAnthropicUsageParserRejectsDuplicateAndOverflow(t *testing.T) {
	for _, body := range []string{`{"usage":{"input_tokens":1,"input_tokens":2,"output_tokens":1}}`, `{"usage":{"input_tokens":9223372036854775807,"cache_read_input_tokens":1,"output_tokens":1}}`, `{"usage":{"input_tokens":1}}`} {
		if _, err := extractUsage([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
