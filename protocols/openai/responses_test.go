package openai

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/chatbilling"
	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	responsesoperation "github.com/nativegatewayhq/gateway/operations/responses"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type responsesExecutorFunc func(context.Context, openaiProvider.ResponsesRequest) (*http.Response, error)

func (f responsesExecutorFunc) Create(ctx context.Context, r openaiProvider.ResponsesRequest) (*http.Response, error) {
	return f(ctx, r)
}

type responsesBillingFake struct {
	begin       chatbilling.BeginRequest
	charge      chatbilling.Charge
	usage       chatpricing.Usage
	released    bool
	reconciling string
}

func (f *responsesBillingFake) Begin(_ context.Context, request chatbilling.BeginRequest) (chatbilling.Charge, error) {
	f.begin = request
	f.charge = chatbilling.Charge{ID: "chc_00000000000000000000000000000001", Operation: request.Operation, MaximumInputTokens: request.MaximumInputTokens, MaximumOutputTokens: request.MaximumOutputTokens, State: "RESERVED"}
	return f.charge, nil
}
func (f *responsesBillingFake) Replay(context.Context, chatbilling.BeginRequest) (chatbilling.Charge, bool, error) {
	return chatbilling.Charge{}, false, nil
}
func (f *responsesBillingFake) CompleteUsage(_ context.Context, _ string, usage chatpricing.Usage, snapshot billing.ResponseSnapshot) (chatbilling.Charge, error) {
	f.usage = usage
	f.charge.Response = snapshot
	return f.charge, nil
}
func (f *responsesBillingFake) Release(_ context.Context, _ string, snapshot billing.ResponseSnapshot) (chatbilling.Charge, error) {
	f.released = true
	f.charge.Response = snapshot
	return f.charge, nil
}
func (f *responsesBillingFake) MarkReconciling(_ context.Context, _ string, reason string, _ *billing.ResponseSnapshot) error {
	f.reconciling = reason
	return nil
}
func (f *responsesBillingFake) MarkReconcilingUsage(_ context.Context, _ string, reason string, _ *billing.ResponseSnapshot, usage chatpricing.Usage) error {
	f.reconciling, f.usage = reason, usage
	return nil
}
func (f *responsesBillingFake) CompleteStreamUsage(_ context.Context, _ string, usage chatpricing.Usage, _ [32]byte) (chatbilling.Charge, error) {
	f.usage = usage
	return f.charge, nil
}
func (f *responsesBillingFake) MarkStreamReconcilingUsage(_ context.Context, _ string, usage chatpricing.Usage, _ [32]byte) error {
	f.reconciling, f.usage = "settlement_failed", usage
	return nil
}
func (f *responsesBillingFake) MarkStreamReconciling(_ context.Context, _ string, reason, _, category string) error {
	f.reconciling = reason + ":" + category
	return nil
}
func TestResponsesPreservesNativeRequestAndResponse(t *testing.T) {
	registry, _ := responsesoperation.NewRegistry([]string{"gpt-4.1"})
	input := `{"model":"gpt-4.1","input":[{"role":"user","content":[{"type":"input_text","text":"secret"}]}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"future":{"x":1}}`
	output := `{"id":"resp_1","object":"response","output":[{"type":"function_call","arguments":"{\"x\":1}"}]}`
	handler := NewResponsesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, responsesExecutorFunc(func(_ context.Context, r openaiProvider.ResponsesRequest) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte(input)) {
			t.Fatalf("body=%s", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "Authorization": {"provider-secret"}}, Body: io.NopCloser(strings.NewReader(output))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, 4096)
	request := chatRequest(input)
	request.URL.Path = "/v1/responses"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != 200 || w.Body.String() != output || w.Header().Get("Authorization") != "" {
		t.Fatalf("status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
}
func TestResponsesPreservesNativeStream(t *testing.T) {
	registry, _ := responsesoperation.NewRegistry([]string{"gpt-4.1"})
	calls := 0
	stream := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":0,\"delta\":\"hello\"}\n\n"
	handler := NewResponsesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, responsesExecutorFunc(func(_ context.Context, request openaiProvider.ResponsesRequest) (*http.Response, error) {
		calls++
		if !request.Streaming || request.Accept != "text/event-stream" {
			t.Fatalf("request=%+v", request)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, 4096)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1","stream":true}`))
	if w.Code != 200 || calls != 1 || w.Body.String() != stream {
		t.Fatalf("status=%d calls=%d body=%q", w.Code, calls, w.Body.String())
	}
}

func TestBillableResponsesReservesAndCapturesNativeUsage(t *testing.T) {
	registry, _ := responsesoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]responsesoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 100}})
	chargeBilling := &responsesBillingFake{}
	handler := NewBillableResponsesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, responsesExecutorFunc(func(context.Context, openaiProvider.ResponsesRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_1","usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":3},"output_tokens":8,"output_tokens_details":{"reasoning_tokens":5}}}`))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, 4096, chargeBilling)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1","input":"hello","max_output_tokens":20}`))
	if w.Code != 200 || chargeBilling.begin.Operation != responsesoperation.Create || chargeBilling.begin.MaximumOutputTokens != 20 || chargeBilling.usage != (chatpricing.Usage{PromptTokens: 10, CachedInputTokens: 3, CompletionTokens: 8}) {
		t.Fatalf("status=%d begin=%+v usage=%+v body=%s", w.Code, chargeBilling.begin, chargeBilling.usage, w.Body.String())
	}
}

func TestBillableResponsesInvalidUsageHoldsReservation(t *testing.T) {
	registry, _ := responsesoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]responsesoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 100}})
	chargeBilling := &responsesBillingFake{}
	handler := NewBillableResponsesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, responsesExecutorFunc(func(context.Context, openaiProvider.ResponsesRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_1"}`))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, 4096, chargeBilling)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1","input":"hello","max_output_tokens":20}`))
	if w.Code != 200 || chargeBilling.reconciling != "usage_invalid" || chargeBilling.released {
		t.Fatalf("status=%d reconciliation=%q released=%v", w.Code, chargeBilling.reconciling, chargeBilling.released)
	}
}

func TestResponsesOutputLimitRejectsDuplicate(t *testing.T) {
	if _, err := extractResponsesOutputLimit([]byte(`{"model":"gpt-4.1","max_output_tokens":10,"max_output_tokens":20}`)); err == nil {
		t.Fatal("duplicate max_output_tokens accepted")
	}
}

func TestBillableResponsesProviderErrorReleases(t *testing.T) {
	registry, _ := responsesoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]responsesoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 100}})
	chargeBilling := &responsesBillingFake{}
	handler := NewBillableResponsesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, responsesExecutorFunc(func(context.Context, openaiProvider.ResponsesRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"limited"}}`))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, 4096, chargeBilling)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1","input":"hello","max_output_tokens":20}`))
	if w.Code != 429 || !chargeBilling.released {
		t.Fatalf("status=%d released=%v", w.Code, chargeBilling.released)
	}
}

func TestBillableResponsesStreamCapturesCompletedUsage(t *testing.T) {
	registry, _ := responsesoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]responsesoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 100}})
	chargeBilling := &responsesBillingFake{}
	stream := completedResponsesStream()
	handler := NewBillableResponsesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, responsesExecutorFunc(func(_ context.Context, request openaiProvider.ResponsesRequest) (*http.Response, error) {
		if !request.Streaming {
			t.Fatal("streaming flag missing")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, 8192, chargeBilling)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1","input":"hello","stream":true,"max_output_tokens":20}`))
	if w.Code != 200 || w.Body.String() != stream || chargeBilling.begin.DeliveryMode != "stream" || chargeBilling.begin.Operation != responsesoperation.Create || chargeBilling.usage != (chatpricing.Usage{PromptTokens: 10, CachedInputTokens: 3, CompletionTokens: 8}) || chargeBilling.reconciling != "" {
		t.Fatalf("status=%d begin=%+v usage=%+v reconciliation=%q body=%q", w.Code, chargeBilling.begin, chargeBilling.usage, chargeBilling.reconciling, w.Body.String())
	}
}

func TestBillableResponsesFailedEventHoldsReservation(t *testing.T) {
	registry, _ := responsesoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]responsesoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 100}})
	chargeBilling := &responsesBillingFake{}
	stream := "event: response.failed\ndata: {\"type\":\"response.failed\",\"sequence_number\":0,\"response\":{\"status\":\"failed\"}}\n\n"
	handler := NewBillableResponsesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, responsesExecutorFunc(func(context.Context, openaiProvider.ResponsesRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, 4096, chargeBilling)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1","input":"hello","stream":true,"max_output_tokens":20}`))
	if w.Code != 200 || chargeBilling.reconciling != "stream_protocol_invalid:response_failed" || chargeBilling.released {
		t.Fatalf("status=%d reconciliation=%q released=%v", w.Code, chargeBilling.reconciling, chargeBilling.released)
	}
}

func TestBillableResponsesClientWriteFailureHoldsReservation(t *testing.T) {
	registry, _ := responsesoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]responsesoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 100}})
	chargeBilling := &responsesBillingFake{}
	handler := NewBillableResponsesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, responsesExecutorFunc(func(context.Context, openaiProvider.ResponsesRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(completedResponsesStream()))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, 8192, chargeBilling)
	handler.ServeHTTP(&disconnectWriter{}, chatRequest(`{"model":"gpt-4.1","input":"hello","stream":true,"max_output_tokens":20}`))
	if chargeBilling.reconciling != "stream_write_failed:write_failed" || chargeBilling.released {
		t.Fatalf("reconciliation=%q released=%v", chargeBilling.reconciling, chargeBilling.released)
	}
}
