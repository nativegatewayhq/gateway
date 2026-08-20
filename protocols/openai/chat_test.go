package openai

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	chatoperation "github.com/nativegatewayhq/gateway/operations/chat"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type chatBillingFake struct {
	beginCalls, completeCalls, releaseCalls, reconcileCalls int
	charge                                                  chatbilling.Charge
	usage                                                   chatpricing.Usage
}

func (f *chatBillingFake) Begin(_ context.Context, r chatbilling.BeginRequest) (chatbilling.Charge, error) {
	f.beginCalls++
	f.charge = chatbilling.Charge{ID: "chc_00000000000000000000000000000001", MaximumInputTokens: r.MaximumInputTokens, MaximumOutputTokens: r.MaximumOutputTokens}
	return f.charge, nil
}
func (f *chatBillingFake) Replay(context.Context, chatbilling.BeginRequest) (chatbilling.Charge, bool, error) {
	return chatbilling.Charge{}, false, nil
}
func (f *chatBillingFake) CompleteUsage(_ context.Context, _ string, u chatpricing.Usage, s billing.ResponseSnapshot) (chatbilling.Charge, error) {
	f.completeCalls++
	f.usage = u
	f.charge.Response = s
	return f.charge, nil
}
func (f *chatBillingFake) Release(_ context.Context, _ string, s billing.ResponseSnapshot) (chatbilling.Charge, error) {
	f.releaseCalls++
	f.charge.Response = s
	return f.charge, nil
}
func (f *chatBillingFake) MarkReconciling(context.Context, string, string, *billing.ResponseSnapshot) error {
	f.reconcileCalls++
	return nil
}
func (f *chatBillingFake) MarkReconcilingUsage(context.Context, string, string, *billing.ResponseSnapshot, chatpricing.Usage) error {
	f.reconcileCalls++
	return nil
}

type chatExecutorFunc func(context.Context, openaiProvider.ChatRequest) (*http.Response, error)

func (f chatExecutorFunc) Complete(ctx context.Context, r openaiProvider.ChatRequest) (*http.Response, error) {
	return f(ctx, r)
}
func chatRegistry(t *testing.T) *chatoperation.Registry {
	t.Helper()
	r, err := chatoperation.NewRegistry([]string{"gpt-4.1"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func chatHandler(t *testing.T, auth Authenticator, executor ChatExecutor, maximum int64) *ChatHandler {
	t.Helper()
	return NewChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), auth, chatRegistry(t), executor, channelAvailability{"channel_00000000000000000000000000000001": true}, providerhealth.NoopGate{}, maximum)
}
func chatRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer service-secret")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestChatPreservesNativeBodyAndResponse(t *testing.T) {
	input := `{"model":"gpt-4.1","messages":[{"role":"user","content":"secret prompt"}],"future_field":{"x":1},"stream":false}`
	calls := 0
	handler := chatHandler(t, acceptingAuth(t), chatExecutorFunc(func(_ context.Context, r openaiProvider.ChatRequest) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte(input)) {
			t.Fatalf("body=%s", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"secret=x"}, "Authorization": {"provider-secret"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl_1","choices":[]}`))}, nil
	}), 4096)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(input))
	if w.Code != 200 || calls != 1 || w.Body.String() != `{"id":"chatcmpl_1","choices":[]}` || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Authorization") != "" {
		t.Fatalf("status=%d calls=%d headers=%v body=%q", w.Code, calls, w.Header(), w.Body.String())
	}
}

func TestChatRejectsBeforeDispatch(t *testing.T) {
	calls := 0
	executor := chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) { calls++; return nil, nil })
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: "image.generate", Model: "gpt-4.1"}}}
	tests := []struct {
		name, body string
		mutate     func(*http.Request)
		status     int
	}{{"stream", `{"model":"gpt-4.1","stream":true}`, nil, 400}, {"trailing", `{"model":"gpt-4.1"}{}`, nil, 400}, {"missing model", `{"messages":[]}`, nil, 400}, {"compressed", `{"model":"gpt-4.1"}`, func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, 415}, {"unauthorized model", `{"model":"gpt-4.1"}`, nil, 403}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := acceptingAuth(t)
			if test.name == "unauthorized model" {
				auth = authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil })
			}
			handler := chatHandler(t, auth, executor, 4096)
			r := chatRequest(test.body)
			if test.mutate != nil {
				test.mutate(r)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	if calls != 0 {
		t.Fatalf("provider calls=%d", calls)
	}
}

func TestChatBoundsAndMapsExecutorFailures(t *testing.T) {
	handler := chatHandler(t, acceptingAuth(t), chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return nil, openaiProvider.ErrChatTimeout
	}), 64)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1"}`))
	if w.Code != 504 {
		t.Fatalf("timeout=%d", w.Code)
	}
	large := chatHandler(t, acceptingAuth(t), chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return nil, errors.New("unexpected")
	}), 8)
	w = httptest.NewRecorder()
	large.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1"}`))
	if w.Code != 413 {
		t.Fatalf("large=%d", w.Code)
	}
	responseLarge := chatHandler(t, acceptingAuth(t), chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 100)))}, nil
	}), 64)
	w = httptest.NewRecorder()
	responseLarge.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1"}`))
	if w.Code != 502 {
		t.Fatalf("large response=%d", w.Code)
	}
}

func TestBillableChatReservesThenSettlesNativeUsage(t *testing.T) {
	registry, err := chatoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]chatoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	billingFake := &chatBillingFake{}
	dispatchedAfterReserve := false
	handler := NewBillableChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		dispatchedAfterReserve = billingFake.beginCalls == 1
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl_1","choices":[],"usage":{"prompt_tokens":12,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens":4}}`))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, providerhealth.NoopGate{}, 4096, billingFake)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1","messages":[],"max_completion_tokens":20}`))
	if w.Code != 200 || !dispatchedAfterReserve || billingFake.completeCalls != 1 || billingFake.usage != (chatpricing.Usage{PromptTokens: 12, CachedInputTokens: 3, CompletionTokens: 4}) {
		t.Fatalf("status=%d reserve=%d complete=%d usage=%+v", w.Code, billingFake.beginCalls, billingFake.completeCalls, billingFake.usage)
	}
}

func TestBillableChatRejectsMissingOutputLimitBeforeReserve(t *testing.T) {
	registry, _ := chatoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]chatoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 1024}})
	billingFake := &chatBillingFake{}
	calls := 0
	handler := NewBillableChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) { calls++; return nil, nil }), channelAvailability{"channel_00000000000000000000000000000001": true}, providerhealth.NoopGate{}, 4096, billingFake)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1","messages":[]}`))
	if w.Code != 400 || calls != 0 || billingFake.beginCalls != 0 {
		t.Fatalf("status=%d provider=%d reserve=%d", w.Code, calls, billingFake.beginCalls)
	}
}

func TestExtractChatUsageRejectsMalformedValues(t *testing.T) {
	for _, body := range []string{`{}`, `{"usage":{"prompt_tokens":1.5,"completion_tokens":1}}`, `{"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens":1}}`} {
		if _, err := extractChatUsage([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
